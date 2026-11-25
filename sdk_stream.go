package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

var streamRoute = regexp.MustCompile(`^/[A-Za-z0-9_-]+(?:/[A-Za-z0-9_-]+)*$`)

type sdkStreamConfig struct {
	sdkLogin
	Stream string
	FPS    int
}

func rtspPublishAddress(value string) (*url.URL, error) {
	target, err := url.Parse(value)
	if err != nil || target.Scheme != "rtsp" || target.User != nil || target.RawQuery != "" ||
		target.Fragment != "" || target.Opaque != "" || target.RawPath != "" ||
		path.Clean(target.Path) != target.Path || !streamRoute.MatchString(target.Path) {
		return nil, errors.New("推流地址须为本机 RTSP 地址，例如 rtsp://127.0.0.1:8554/camera/front")
	}
	ip := net.ParseIP(target.Hostname())
	port, err := strconv.Atoi(target.Port())
	if ip == nil || !ip.IsLoopback() || err != nil || port < 1 || port > 65535 {
		return nil, errors.New("RTSP 推流仅支持本机回环 IP 和显式端口")
	}
	return target, nil
}

func ffmpegStreamArgs(format string, fps int, address string) []string {
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-nostats",
		"-progress", "pipe:1", "-stats_period", "1",
		"-fflags", "+genpts", "-probesize", "524288", "-analyzeduration", "2000000",
	}
	if format == "h264" {
		args = append(args, "-r", strconv.Itoa(fps))
	}
	return append(args, "-f", format, "-i", "pipe:0", "-map", "0:v:0", "-c:v", "copy",
		"-f", "rtsp", "-rtsp_transport", "tcp", address)
}

// A process can emit large or unrelated lines. Retain only bounded protocol
// fields and discard everything else, including SDK and FFmpeg diagnostics.
type processLines struct {
	line     []byte
	overflow bool
	receive  func(string)
}

func (p *processLines) Write(bytes []byte) (int, error) {
	for _, b := range bytes {
		switch {
		case b == '\n':
			if !p.overflow {
				p.receive(strings.TrimRight(string(p.line), "\r"))
			}
			p.line = p.line[:0]
			p.overflow = false
		case p.overflow:
		case len(p.line) >= 255:
			p.overflow = true
		default:
			p.line = append(p.line, b)
		}
	}
	return len(bytes), nil
}

func sdkVideoFormat(ctx context.Context, reader *bufio.Reader) (string, error) {
	line := make(chan string, 1)
	go func() {
		data, _ := reader.ReadSlice('\n')
		line <- string(data)
	}()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case header := <-line:
		switch header {
		case "JIAJING-VIDEO/1 mpeg\n":
			return "mpeg", nil
		case "JIAJING-VIDEO/1 h264\n":
			return "h264", nil
		default:
			return "", errors.New("SDK 适配器没有返回可用的视频流，请确认已编译最新适配器")
		}
	case <-timer.C:
		return "", errors.New("等待原生 SDK 开始取流超时")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func relaySDKVideo(ctx context.Context, adapter, ffmpeg string, config sdkStreamConfig, address string) (result error) {
	active, cancel := context.WithCancel(ctx)
	sdk := exec.CommandContext(active, adapter, "--stream-"+config.Stream)
	sdk.Stdin = strings.NewReader(config.adapterInput())
	sdk.Cancel = func() error {
		err := sdk.Process.Signal(os.Interrupt)
		if err != nil && !errors.Is(err, os.ErrProcessDone) {
			return sdk.Process.Kill()
		}
		return err
	}
	sdk.WaitDelay = 3 * time.Second
	var sdkError atomic.Int64
	sdkError.Store(-1)
	sdk.Stderr = &processLines{receive: func(line string) {
		if value, ok := strings.CutPrefix(line, adapterPrefix+"ERROR\t"); ok {
			if number, err := strconv.ParseUint(value, 10, 32); err == nil {
				sdkError.Store(int64(number))
			}
		}
	}}
	output, err := sdk.StdoutPipe()
	if err != nil {
		cancel()
		return errors.New("无法打开 SDK 视频输出")
	}
	if err := sdk.Start(); err != nil {
		output.Close()
		cancel()
		return errors.New("无法启动 SDK 视频适配器，请检查可执行文件和厂商库")
	}
	defer func() {
		cancel()
		output.Close()
		sdk.Wait()
		if code := sdkError.Load(); code >= 0 && result != nil && ctx.Err() == nil {
			result = sdkOperationError(code)
		}
	}()

	reader := bufio.NewReaderSize(output, 64)
	format, err := sdkVideoFormat(active, reader)
	if err != nil {
		return err
	}
	encode := exec.CommandContext(active, ffmpeg, ffmpegStreamArgs(format, config.FPS, address)...)
	encode.WaitDelay = 3 * time.Second
	encode.Stdin = reader
	var frames atomic.Int64
	encode.Stdout = &processLines{receive: func(line string) {
		if raw, ok := strings.CutPrefix(line, "frame="); ok {
			if count, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil {
				frames.Store(count)
			}
		}
	}}
	encode.Stderr = io.Discard
	if err := encode.Start(); err != nil {
		return errors.New("无法启动 FFmpeg")
	}
	finished := make(chan struct{})
	go func() {
		encode.Wait()
		close(finished)
	}()
	defer func() {
		cancel()
		output.Close()
		<-finished
	}()

	lastFrames := int64(0)
	deadline := time.Now().Add(20 * time.Second)
	check := time.NewTicker(time.Second)
	defer check.Stop()
	for {
		select {
		case <-active.Done():
			return active.Err()
		case <-check.C:
			if count := frames.Load(); count > lastFrames {
				if lastFrames == 0 {
					log.Print("原生 SDK 视频已发布到本机 RTSP")
				}
				lastFrames = count
				deadline = time.Now().Add(10 * time.Second)
			}
			if time.Now().After(deadline) {
				return errors.New("SDK 视频帧停止更新，请检查摄像头编码、码流和本机 RTSP 服务")
			}
		case <-finished:
			return errors.New("RTSP 推流已停止，请检查接收端和设备码流")
		}
	}
}

func runSDKStream(args []string) error {
	flags := flag.NewFlagSet("sdk-stream", flag.ContinueOnError)
	adapter := flags.String("adapter", "", "原生 SDK 适配器的可执行文件")
	configPath := flags.String("config", "", "权限为 0600 的原生 SDK 取流配置 JSON")
	ffmpeg := flags.String("ffmpeg", "ffmpeg", "FFmpeg 可执行文件")
	publish := flags.String("publish", "", "本机 RTSP 接收地址")
	if err := flags.Parse(args); err != nil {
		return err
	}
	helper, err := exec.LookPath(*adapter)
	if err != nil {
		return errors.New("请用 -adapter 指定已安装的原生 SDK 适配器")
	}
	videoTool, err := exec.LookPath(*ffmpeg)
	if err != nil {
		return errors.New("请安装 FFmpeg 或用 -ffmpeg 指定路径")
	}
	address, err := rtspPublishAddress(*publish)
	if err != nil {
		return err
	}
	value, err := readPrivate(*configPath)
	if err != nil {
		return errors.New("请用 -config 指定仅当前用户可读取的 SDK 取流配置文件")
	}
	config := sdkStreamConfig{Stream: "main", FPS: 15, sdkLogin: sdkLogin{DeviceKind: "ipc"}}
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&config) != nil || (config.Stream != "main" && config.Stream != "sub") ||
		config.FPS < 1 || config.FPS > 120 {
		return errors.New("SDK 取流配置无效，码流应为 main 或 sub，帧率范围为 1 到 120")
	}
	if err := config.sdkLogin.validate(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for ctx.Err() == nil {
		if err := relaySDKVideo(ctx, helper, videoTool, config, address.String()); err != nil && ctx.Err() == nil {
			var operation sdkOperationError
			if errors.As(err, &operation) {
				return err
			}
			log.Printf("原生 SDK 推流中断，5 秒后重试 (%s)", err)
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
		}
	}
	return nil
}
