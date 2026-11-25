package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	addressingNS = "http://www.w3.org/2005/08/addressing"
	notifyNS     = "http://docs.oasis-open.org/wsn/b-2"
	subManager   = "http://docs.oasis-open.org/wsn/bw-2/SubscriptionManager/"
	signalLease  = 60 * time.Second
)

type onvifSubscription struct {
	camera   *onvifDevice
	endpoint *url.URL
	headers  string
}

func (d *onvifDevice) subscribe(ctx context.Context) (*onvifSubscription, error) {
	services, err := d.services(ctx, eventsNS)
	if err != nil {
		return nil, err
	}
	for _, service := range services {
		if service.Namespace != eventsNS || service.Address == "" {
			continue
		}
		endpoint, err := cameraService(d.endpoint, service.Address, false)
		if err != nil {
			return nil, err
		}
		data, err := d.addressed(ctx, endpoint, "", eventsNS, "CreatePullPointSubscription",
			eventsNS+"/EventPortType/CreatePullPointSubscriptionRequest",
			"<m:CreatePullPointSubscription><m:InitialTerminationTime>PT90S</m:InitialTerminationTime></m:CreatePullPointSubscription>")
		if err != nil {
			return nil, err
		}
		var response struct {
			Address string       `xml:"Body>CreatePullPointSubscriptionResponse>SubscriptionReference>Address"`
			Params  xmlReference `xml:"Body>CreatePullPointSubscriptionResponse>SubscriptionReference>ReferenceParameters"`
		}
		if err := xml.Unmarshal(data, &response); err != nil || response.Address == "" {
			return nil, errors.New("设备没有返回 ONVIF 订阅地址")
		}
		target, err := cameraService(endpoint, response.Address, false)
		if err != nil {
			return nil, err
		}
		return &onvifSubscription{camera: d, endpoint: target, headers: response.Params.encoded}, nil
	}
	return nil, errors.New("设备未提供 ONVIF Events 服务")
}

func (d *onvifDevice) addressed(ctx context.Context, endpoint *url.URL, refs, ns, action, uri, body string) ([]byte, error) {
	id, err := onvifMessageID()
	if err != nil {
		return nil, errors.New("无法生成 ONVIF 消息标识")
	}
	headers := "<wsa:Action>" + uri + "</wsa:Action><wsa:To>" + xmlText(endpoint.String()) +
		"</wsa:To><wsa:MessageID>" + id + "</wsa:MessageID><wsa:ReplyTo><wsa:Address>" + addressingNS +
		"/anonymous</wsa:Address></wsa:ReplyTo>" + refs
	return d.soapRequest(ctx, endpoint, ns, action, uri, body, headers)
}

type xmlReference struct{ encoded string }

// Re-encode reference parameters with their original namespaces. Their
// prefixes may have been declared on outer elements of the camera response.
func (r *xmlReference) UnmarshalXML(dec *xml.Decoder, _ xml.StartElement) error {
	var data bytes.Buffer
	enc := xml.NewEncoder(&data)
	depth := 0
	for {
		entry, err := dec.Token()
		if err != nil {
			return err
		}
		switch element := entry.(type) {
		case xml.StartElement:
			var attrs []xml.Attr
			for _, attr := range element.Attr {
				if attr.Name.Space != "xmlns" && attr.Name.Local != "xmlns" &&
					!(attr.Name.Space == addressingNS && attr.Name.Local == "IsReferenceParameter") {
					attrs = append(attrs, attr)
				}
			}
			if depth == 0 {
				attrs = append(attrs, xml.Attr{Name: xml.Name{Space: addressingNS, Local: "IsReferenceParameter"}, Value: "true"})
			}
			element.Attr = attrs
			entry = element
			depth++
		case xml.EndElement:
			if depth == 0 {
				if err := enc.Flush(); err != nil {
					return err
				}
				r.encoded = data.String()
				return nil
			}
			depth--
		}
		if err := enc.EncodeToken(entry); err != nil {
			return err
		}
	}
}

type onvifSimpleItem struct {
	Name  string `xml:"Name,attr"`
	Value string `xml:"Value,attr"`
}

type onvifNotification struct {
	Topic   string `xml:"Topic"`
	Message struct {
		UTC       string            `xml:"UtcTime,attr"`
		Operation string            `xml:"PropertyOperation,attr"`
		Source    []onvifSimpleItem `xml:"Source>SimpleItem"`
		Data      []onvifSimpleItem `xml:"Data>SimpleItem"`
	} `xml:"Message>Message"`
}

func (s *onvifSubscription) pull(ctx context.Context) ([]onvifNotification, error) {
	data, err := s.camera.addressed(ctx, s.endpoint, s.headers, eventsNS, "PullMessages",
		eventsNS+"/PullPointSubscription/PullMessagesRequest",
		"<m:PullMessages><m:Timeout>PT4S</m:Timeout><m:MessageLimit>16</m:MessageLimit></m:PullMessages>")
	if err != nil {
		return nil, err
	}
	var response struct {
		Items []onvifNotification `xml:"Body>PullMessagesResponse>NotificationMessage"`
	}
	if err := xml.Unmarshal(data, &response); err != nil {
		return nil, errors.New("设备返回了无效的 ONVIF 告警数据")
	}
	return response.Items, nil
}

func (s *onvifSubscription) renew(ctx context.Context) error {
	_, err := s.camera.addressed(ctx, s.endpoint, s.headers, notifyNS, "Renew", subManager+"RenewRequest",
		"<m:Renew><m:TerminationTime>PT90S</m:TerminationTime></m:Renew>")
	return err
}

func (s *onvifSubscription) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.camera.addressed(ctx, s.endpoint, s.headers, notifyNS, "Unsubscribe", subManager+"UnsubscribeRequest", "<m:Unsubscribe/>")
}

func (n onvifNotification) motion(sourceFilter string) (string, bool, bool) {
	topic := strings.TrimSpace(n.Topic)
	parts := strings.Split(topic, "/")
	leaf := parts[len(parts)-1]
	if at := strings.LastIndexByte(leaf, ':'); at >= 0 {
		leaf = leaf[at+1:]
	}
	switch strings.ToLower(leaf) {
	case "motion", "motionalarm", "motiondetection", "motiondetected":
	default:
		return "", false, false
	}
	source := make([]string, 0, len(n.Message.Source))
	matched := sourceFilter == ""
	for _, item := range n.Message.Source {
		source = append(source, item.Name+"="+item.Value)
		matched = matched || item.Value == sourceFilter
	}
	if !matched {
		return "", false, false
	}
	sort.Strings(source)
	key := topic + "\x00" + strings.Join(source, "\x00")
	if strings.EqualFold(n.Message.Operation, "Deleted") {
		return key, false, true
	}
	for _, item := range n.Message.Data {
		switch strings.ToLower(item.Name) {
		case "state", "ismotion", "motion", "motionalarm", "ismotiondetected":
			switch strings.ToLower(strings.TrimSpace(item.Value)) {
			case "true", "1", "on", "active":
				return key, true, true
			case "false", "0", "off", "inactive":
				return key, false, true
			}
		}
	}
	return "", false, false
}

type motionConfig struct {
	Endpoint    string
	Username    string
	Password    string
	SourceToken string
	SignalID    int32
	MotionState uint8
	StillState  uint8
}

type signalWriter struct {
	bridge *bridge
	csrf   string
	config motionConfig
	last   int64
}

func (s *signalWriter) csrfToken(ctx context.Context) error {
	var result struct {
		User struct {
			Session struct {
				CSRF string `json:"csrf"`
			} `json:"session"`
		} `json:"user"`
		Permissions struct {
			UpdateSignals bool `json:"updateSignals"`
		} `json:"permissions"`
	}
	target := s.bridge.requestURL("/api/", nil)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if s.bridge.cookie != "" {
		req.Header.Set("Cookie", s.bridge.cookie)
	}
	res, err := s.bridge.client.Do(req)
	if err != nil {
		return errors.New("无法连接 Moonfire")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(&result) != nil ||
		!result.Permissions.UpdateSignals {
		return errors.New("Moonfire 连接需要 updateSignals 权限")
	}
	s.csrf = result.User.Session.CSRF
	return nil
}

func (s *signalWriter) write(ctx context.Context, moving bool, start time.Time) error {
	state := s.config.StillState
	if moving {
		state = s.config.MotionState
	}
	begin := max(start.UnixMilli()*90, s.last+1)
	end := time.Now().Add(signalLease).UnixMilli() * 90
	type instant struct {
		Base string `json:"base"`
		Time int64  `json:"rel90k"`
	}
	payload := struct {
		CSRF  string  `json:"csrf,omitempty"`
		IDs   []int32 `json:"signalIds"`
		State []int   `json:"states"`
		Start instant `json:"start"`
		End   instant `json:"end"`
	}{s.csrf, []int32{s.config.SignalID}, []int{int(state)}, instant{"epoch", begin}, instant{"epoch", end}}
	var body bytes.Buffer
	json.NewEncoder(&body).Encode(payload)
	target := s.bridge.requestURL("/api/signals", nil)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), &body)
	req.Header.Set("Content-Type", "application/json")
	if s.bridge.cookie != "" {
		req.Header.Set("Cookie", s.bridge.cookie)
	}
	res, err := s.bridge.client.Do(req)
	if err != nil {
		return errors.New("无法将告警写入 Moonfire")
	}
	defer res.Body.Close()
	io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("Moonfire 拒绝告警更新 (HTTP %d)", res.StatusCode)
	}
	s.last = begin
	return nil
}

func motionTime(item onvifNotification, now time.Time) time.Time {
	timestamp, err := time.Parse(time.RFC3339Nano, item.Message.UTC)
	if err != nil || timestamp.Before(now.Add(-30*time.Second)) || timestamp.After(now) {
		return now
	}
	return timestamp
}

func relayONVIF(ctx context.Context, camera *onvifDevice, writer *signalWriter) error {
	if err := writer.csrfToken(ctx); err != nil {
		return err
	}
	subscription, err := camera.subscribe(ctx)
	if err != nil {
		return err
	}
	defer subscription.close()
	log.Print("已订阅 ONVIF 移动事件")
	renewAt := time.Now().Add(40 * time.Second)
	known, moving := false, false
	lastPublish := time.Time{}
	rules := make(map[string]bool)
	for ctx.Err() == nil {
		if !time.Now().Before(renewAt) {
			if err := subscription.renew(ctx); err != nil {
				return err
			}
			renewAt = time.Now().Add(40 * time.Second)
		}
		start := time.Now()
		items, err := subscription.pull(ctx)
		if err != nil {
			return err
		}
		for _, item := range items {
			key, state, ok := item.motion(writer.config.SourceToken)
			if !ok {
				continue
			}
			if state {
				if len(rules) >= 256 && !rules[key] {
					continue
				}
				rules[key] = true
			} else {
				delete(rules, key)
			}
			current := len(rules) != 0
			if !known || current != moving {
				if err := writer.write(ctx, current, motionTime(item, time.Now())); err != nil {
					return err
				}
				moving, known, lastPublish = current, true, time.Now()
			}
		}
		if known && time.Since(lastPublish) > 20*time.Second {
			if err := writer.write(ctx, moving, time.Now()); err != nil {
				return err
			}
			lastPublish = time.Now()
		}
		// Some cameras immediately return an empty response.
		if len(items) == 0 && time.Since(start) < 200*time.Millisecond {
			select {
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
			}
		}
	}
	return ctx.Err()
}

func runONVIFEvents(args []string) error {
	flags := flag.NewFlagSet("onvif-events", flag.ContinueOnError)
	configPath := flags.String("config", "", "权限为 0600 的 ONVIF 告警配置 JSON")
	target := flags.String("moonfire", "127.0.0.1:8080", "Moonfire 本机地址，或 unix:/path/to/socket")
	cookieFile := flags.String("cookie-file", "", "仅有 updateSignals 权限的 Moonfire cookie jar")
	if err := flags.Parse(args); err != nil {
		return err
	}
	contents, err := readPrivate(*configPath)
	if err != nil {
		return errors.New("请通过 -config 指定仅当前用户可读取的 ONVIF 配置文件")
	}
	config := motionConfig{MotionState: 2, StillState: 1}
	decoder := json.NewDecoder(strings.NewReader(contents))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&config) != nil || config.SignalID < 1 || config.MotionState < 1 || config.MotionState > 15 ||
		config.StillState < 1 || config.StillState > 15 || config.MotionState == config.StillState {
		return errors.New("ONVIF 告警配置无效，请检查摄像头地址、信号 ID 和状态值")
	}
	camera, err := newONVIF(config.Endpoint, config.Username, config.Password)
	if err != nil {
		return err
	}
	defer camera.client.CloseIdleConnections()
	b, err := newBridge("", *target, *cookieFile)
	if err != nil {
		return err
	}
	writer := &signalWriter{bridge: b, config: config}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for ctx.Err() == nil {
		if err := relayONVIF(ctx, camera, writer); err != nil && ctx.Err() == nil {
			log.Printf("ONVIF 告警连接中断，5 秒后重试 (%s)", err)
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
		}
	}
	return nil
}
