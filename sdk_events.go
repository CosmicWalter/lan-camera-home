package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const adapterPrefix = "JIAJING\t1\t"

type sdkOperationError uint32

func (err sdkOperationError) Error() string {
	return fmt.Sprintf("原生 SDK 操作失败（错误码 %d）", uint32(err))
}

type sdkConfig struct {
	Host              string
	Port              uint16
	Username          string
	Password          string
	Channel           int
	DeviceKind        string
	SignalID          int32
	StillState        uint8
	MotionState       uint8
	MotionHoldSeconds int
}

func (c *sdkConfig) validate() error {
	ip, err := netip.ParseAddr(c.Host)
	if err != nil || !localIP(ip.Unmap()) || ip.Zone() != "" || c.Port == 0 || c.Channel < 1 || c.Channel > 4096 {
		return errors.New("请填写本地设备 IP、SDK 端口和通道号")
	}
	if c.Username == "" || len(c.Username) > 256 || len(c.Password) > 512 ||
		strings.ContainsAny(c.Username+c.Password, "\r\n\x00") {
		return errors.New("SDK 登录凭据的长度或字符无效")
	}
	if c.DeviceKind != "ipc" && c.DeviceKind != "nvr" {
		return errors.New("设备类型须为 ipc 或 nvr")
	}
	if c.SignalID < 1 || c.StillState < 1 || c.StillState > 15 || c.MotionState < 1 ||
		c.MotionState > 15 || c.StillState == c.MotionState || c.MotionHoldSeconds < 1 || c.MotionHoldSeconds > 120 {
		return errors.New("请检查信号 ID、状态值和移动保持秒数")
	}
	c.Host = ip.Unmap().String()
	return nil
}

func (c sdkConfig) adapterInput() string {
	return strings.Join([]string{
		"JIAJING/1", c.Host, strconv.Itoa(int(c.Port)), c.Username, c.Password,
		strconv.Itoa(c.Channel), c.DeviceKind, "",
	}, "\n")
}

func relaySDK(ctx context.Context, adapter string, config sdkConfig, writer *signalWriter) error {
	if err := writer.csrfToken(ctx); err != nil {
		return err
	}
	processCtx, stop := context.WithCancel(ctx)
	cmd := exec.CommandContext(processCtx, adapter)
	cmd.Stdin = strings.NewReader(config.adapterInput())
	if runtime.GOOS != "windows" {
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	}
	cmd.WaitDelay = 3 * time.Second
	output, err := cmd.StdoutPipe()
	if err != nil {
		stop()
		return errors.New("无法打开原生 SDK 适配器输出")
	}
	if err := cmd.Start(); err != nil {
		stop()
		return errors.New("无法启动原生 SDK 适配器，请检查可执行文件和依赖")
	}
	defer func() {
		stop()
		cmd.Wait()
	}()

	messages := make(chan string, 8)
	go func() {
		defer close(messages)
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			message, ok := strings.CutPrefix(scanner.Text(), adapterPrefix)
			if !ok {
				continue
			}
			select {
			case messages <- message:
			case <-processCtx.Done():
				return
			}
		}
	}()

	ready, moving := false, false
	lastActivity, lastWrite := time.Now(), time.Time{}
	motionUntil := time.Time{}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case message, open := <-messages:
			if !open {
				return errors.New("原生 SDK 适配器已退出")
			}
			now := time.Now()
			switch message {
			case "READY":
				if ready {
					return errors.New("适配器重复发送 SDK 登录状态")
				}
				if err := writer.write(ctx, false, now); err != nil {
					return err
				}
				ready, lastWrite, lastActivity = true, now, now
				log.Print("原生 SDK 已登录并订阅告警")
			case "HEARTBEAT", "MOTION", "MOTION_ON", "MOTION_OFF":
				if !ready {
					return errors.New("原生 SDK 适配器在登录前发送了事件")
				}
				lastActivity = now
				if message == "HEARTBEAT" {
					continue
				}
				motionUntil = time.Time{}
				if message == "MOTION" {
					motionUntil = now.Add(time.Duration(config.MotionHoldSeconds) * time.Second)
				}
				next := message != "MOTION_OFF"
				if next != moving {
					if err := writer.write(ctx, next, now); err != nil {
						return err
					}
					moving, lastWrite = next, now
				}
			case "DISCONNECTED":
				return errors.New("设备与原生 SDK 的告警连接已断开")
			default:
				if code, ok := strings.CutPrefix(message, "ERROR\t"); ok {
					if number, err := strconv.ParseUint(code, 10, 32); err == nil {
						return sdkOperationError(number)
					}
				}
			}
		case now := <-ticker.C:
			if now.Sub(lastActivity) > 12*time.Second {
				return errors.New("原生 SDK 登录或告警心跳超时")
			}
			expired := moving && !motionUntil.IsZero() && !now.Before(motionUntil)
			if ready && (expired || now.Sub(lastWrite) > 20*time.Second) {
				if expired {
					moving = false
				}
				if err := writer.write(ctx, moving, now); err != nil {
					return err
				}
				lastWrite = now
			}
		}
	}
}

func runSDKEvents(args []string) error {
	flags := flag.NewFlagSet("sdk-events", flag.ContinueOnError)
	adapter := flags.String("adapter", "", "原生 SDK 适配器的可执行文件")
	configPath := flags.String("config", "", "权限为 0600 的原生 SDK 告警配置 JSON")
	target := flags.String("moonfire", "127.0.0.1:8080", "Moonfire 本机地址，或 unix:/path/to/socket")
	cookieFile := flags.String("cookie-file", "", "仅有 updateSignals 权限的 Moonfire cookie jar")
	if err := flags.Parse(args); err != nil {
		return err
	}
	path, err := exec.LookPath(*adapter)
	if err != nil {
		return errors.New("请用 -adapter 指定已安装的原生 SDK 适配器")
	}
	contents, err := readPrivate(*configPath)
	if err != nil {
		return errors.New("请用 -config 指定仅当前用户可读取的 SDK 配置文件")
	}
	config := sdkConfig{MotionState: 2, StillState: 1, MotionHoldSeconds: 12, DeviceKind: "ipc"}
	decoder := json.NewDecoder(strings.NewReader(contents))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&config) != nil {
		return errors.New("原生 SDK 配置 JSON 无效")
	}
	if err := config.validate(); err != nil {
		return err
	}
	b, err := newBridge("", *target, *cookieFile)
	if err != nil {
		return err
	}
	writer := &signalWriter{bridge: b, config: motionConfig{
		SignalID: config.SignalID, MotionState: config.MotionState, StillState: config.StillState,
	}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for ctx.Err() == nil {
		if err := relaySDK(ctx, path, config, writer); err != nil && ctx.Err() == nil {
			var operation sdkOperationError
			if errors.As(err, &operation) {
				// Device authentication failures can lock the account after
				// repeated attempts. Vendors use different error enums.
				return err
			}
			log.Printf("原生 SDK 告警连接中断，5 秒后重试 (%s)", err)
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
		}
	}
	return nil
}
