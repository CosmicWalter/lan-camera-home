package main

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const pagesOrigin = "https://cosmicwalter.github.io"
const frameLimit = 16 << 20

//go:embed web
var webFiles embed.FS

var streamPath = regexp.MustCompile(`^/api/cameras/[a-fA-F0-9]{8}-(?:[a-fA-F0-9]{4}-){3}[a-fA-F0-9]{12}/(?:main|sub|ext)/(recordings|live\.m4s|view\.mp4)$`)
var initPath = regexp.MustCompile(`^/api/init/[0-9]+\.mp4$`)
var segment = regexp.MustCompile(`^[0-9]+(?:-[0-9]+)?(?:@[0-9]+)?(?:\.[0-9]*-[0-9]*)?$`)
var cookiePattern = regexp.MustCompile(`^[A-Za-z0-9+/]{64}$`)

type bridge struct {
	host, token, cookie string
	signingKey          []byte
	upstream            *url.URL
	client              *http.Client
	ws                  websocket.Dialer
	static              http.Handler
}

func token() string {
	return base64.RawURLEncoding.EncodeToString(randomBytes())
}

func randomBytes() []byte {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		log.Fatal("无法初始化随机密钥")
	}
	return value
}

func main() {
	port := flag.Int("port", 8787, "本机桥接端口")
	upstream := flag.String("moonfire", "127.0.0.1:8080", "Moonfire 本机地址，或 unix:/path/to/socket")
	cookieFile := flag.String("cookie-file", "", "可选，Moonfire CLI 生成的 curl cookie jar")
	pairingFile := flag.String("pairing-file", "", "可选，保存配对码的私有文件，供服务重启后复用")
	flag.Parse()

	if *port < 1 || *port > 65535 {
		log.Fatal("端口必须在 1 到 65535 之间")
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(*port))
	b, err := newBridge(address, *upstream, *cookieFile)
	if err != nil {
		log.Fatal(err)
	}
	if *pairingFile != "" {
		b.token, err = persistentToken(*pairingFile, b.token)
		if err != nil {
			log.Fatal(err)
		}
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatal("无法监听本机端口，请检查端口占用")
	}
	fmt.Printf("本机控制台  http://%s/\nGitHub Pages  %s/lan-camera-home/\n配对码  %s\n", address, pagesOrigin, b.token)
	server := &http.Server{
		Handler: b, ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	log.Fatal(server.Serve(listener))
}

func persistentToken(path, generated string) (string, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		_, writeErr := fmt.Fprintln(file, generated)
		if errors.Join(writeErr, file.Close()) != nil {
			return "", errors.New("无法保存配对码")
		}
		return generated, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return "", errors.New("无法创建配对码文件")
	}
	value, err := readPrivate(path)
	raw, decodeErr := base64.RawURLEncoding.DecodeString(value)
	if err != nil || decodeErr != nil || len(raw) != 32 {
		return "", errors.New("配对码文件无效，请检查文件权限和内容")
	}
	return value, nil
}

func readPrivate(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("需要一个仅当前用户可访问的普通文件")
	}
	data, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil || len(data) > 8192 {
		return "", errors.New("无法读取凭据文件")
	}
	return strings.TrimSpace(string(data)), nil
}

func newBridge(host, target, cookieFile string) (*bridge, error) {
	dial := (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	upstream := &url.URL{Scheme: "http", Host: target}
	if socket, ok := strings.CutPrefix(target, "unix:"); ok && socket != "" {
		upstream.Host = "localhost"
		socketDial := dial
		dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return socketDial(ctx, "unix", socket)
		}
	} else {
		ip, port, err := net.SplitHostPort(target)
		portNumber, portErr := strconv.Atoi(port)
		if parsed := net.ParseIP(ip); err != nil || parsed == nil || !parsed.IsLoopback() ||
			portErr != nil || portNumber < 1 || portNumber > 65535 || target == host {
			return nil, errors.New("Moonfire 地址必须为本机回环 IP 加端口，或 unix: 套接字")
		}
	}

	var cookie string
	if cookieFile != "" {
		value, err := readPrivate(cookieFile)
		if err != nil {
			return nil, errors.New("请检查 Moonfire cookie 文件和权限")
		}
		for _, line := range strings.Split(value, "\n") {
			parts := strings.Split(strings.TrimSpace(line), "\t")
			if len(parts) == 7 && parts[5] == "s" && cookiePattern.MatchString(parts[6]) {
				cookie = "s=" + parts[6]
				break
			}
		}
		if cookie == "" {
			return nil, errors.New("Moonfire cookie 文件没有有效的 s cookie")
		}
	}

	files, _ := fs.Sub(webFiles, "web")
	return &bridge{
		host: host, token: token(), signingKey: randomBytes(), cookie: cookie,
		upstream: upstream, static: http.FileServer(http.FS(files)),
		client: &http.Client{
			Transport:     &http.Transport{DialContext: dial, ResponseHeaderTimeout: 10 * time.Second, IdleConnTimeout: 30 * time.Second},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		ws: websocket.Dialer{NetDialContext: dial, HandshakeTimeout: 10 * time.Second},
	}, nil
}

func (b *bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	if r.Host != b.host {
		http.Error(w, "无效的本机地址", http.StatusForbidden)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		b.static.ServeHTTP(w, r)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Origin")
	origin := r.Header.Get("Origin")
	if origin != "" && origin != "http://"+b.host && origin != pagesOrigin {
		http.Error(w, "来源不受信任", http.StatusForbidden)
		return
	}
	if origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	}
	if r.Method == http.MethodOptions && origin != "" && r.Header.Get("Access-Control-Request-Method") == "GET" {
		w.Header().Set("Access-Control-Allow-Methods", "GET")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Range")
		if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
			w.Header().Set("Access-Control-Allow-Private-Network", "true")
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	action := ""
	match := streamPath.FindStringSubmatch(r.URL.Path)
	switch {
	case r.URL.Path == "/api/":
		action = "inventory"
	case r.URL.Path == "/api/signals":
		action = "signals"
	case r.URL.Path == "/api/media-ticket":
		action = "ticket"
	case initPath.MatchString(r.URL.Path):
		action = "init"
	case match != nil:
		action = match[1]
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}

	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(r.URL.RawQuery) > 4096 {
		http.Error(w, "查询参数无效", http.StatusBadRequest)
		return
	}

	if action == "view.mp4" {
		b.media(w, r, query)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+b.token)) != 1 {
		http.Error(w, "配对码无效", http.StatusUnauthorized)
		return
	}

	switch action {
	case "ticket":
		b.ticket(w, query)
	case "live.m4s":
		b.live(w, r)
	case "inventory":
		b.proxy(w, r, url.Values{"days": {"true"}}, action, false)
	case "recordings":
		if !numericQuery(query, "startTime90k", "endTime90k", "split90k") {
			http.Error(w, "录像查询范围无效", http.StatusBadRequest)
			return
		}
		b.proxy(w, r, query, action, false)
	case "signals":
		if !numericQuery(query, "startTime90k", "endTime90k") {
			http.Error(w, "事件查询范围无效", http.StatusBadRequest)
			return
		}
		b.proxy(w, r, query, action, false)
	case "init":
		b.proxy(w, r, nil, action, false)
	}
}

func numericQuery(query url.Values, keys ...string) bool {
	if len(query) != len(keys) {
		return false
	}
	for _, key := range keys {
		values := query[key]
		if len(values) != 1 {
			return false
		}
		if value, err := strconv.ParseInt(values[0], 10, 64); err != nil || value < 0 {
			return false
		}
	}
	return query.Get("split90k") != "0"
}

func (b *bridge) signed(path string, query url.Values) string {
	mac := hmac.New(sha256.New, b.signingKey)
	io.WriteString(mac, path+"?"+query.Encode())
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (b *bridge) ticket(w http.ResponseWriter, query url.Values) {
	path, clip, download := query.Get("path"), query.Get("s"), query.Get("download")
	if !streamPath.MatchString(path) || !strings.HasSuffix(path, "/view.mp4") ||
		!segment.MatchString(clip) || (download != "" && download != "1") {
		http.Error(w, "录像片段无效", http.StatusBadRequest)
		return
	}
	values := url.Values{
		"s": {clip}, "exp": {strconv.FormatInt(time.Now().Add(30*time.Minute).Unix(), 10)},
		"download": {download},
	}
	values.Set("sig", b.signed(path, values))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"url": path + "?" + values.Encode()})
}

func (b *bridge) media(w http.ResponseWriter, r *http.Request, query url.Values) {
	signature := query.Get("sig")
	query.Del("sig")
	expires, err := strconv.ParseInt(query.Get("exp"), 10, 64)
	if err != nil || expires < time.Now().Unix() ||
		subtle.ConstantTimeCompare([]byte(signature), []byte(b.signed(r.URL.Path, query))) != 1 {
		http.Error(w, "录像链接已过期或无效", http.StatusUnauthorized)
		return
	}
	b.proxy(w, r, url.Values{"s": {query.Get("s")}}, "view.mp4", query.Get("download") == "1")
}

func (b *bridge) requestURL(path string, query url.Values) url.URL {
	target := *b.upstream
	target.Path, target.RawQuery = path, query.Encode()
	return target
}

func (b *bridge) proxy(w http.ResponseWriter, r *http.Request, query url.Values, action string, download bool) {
	ctx := r.Context()
	if action != "view.mp4" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 12*time.Second)
		defer cancel()
	}
	target := b.requestURL(r.URL.Path, query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if b.cookie != "" {
		req.Header.Set("Cookie", b.cookie)
	}
	if action == "view.mp4" {
		req.Header.Set("Range", r.Header.Get("Range"))
	}
	res, err := b.client.Do(req)
	if err != nil {
		http.Error(w, "无法连接 Moonfire NVR", http.StatusBadGateway)
		return
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
		// Forward only expected successful responses.
	case http.StatusRequestedRangeNotSatisfiable:
		w.Header().Set("Content-Range", res.Header.Get("Content-Range"))
		w.WriteHeader(res.StatusCode)
		return
	case http.StatusUnauthorized, http.StatusForbidden:
		http.Error(w, "Moonfire 拒绝访问，请检查桥接账号权限", http.StatusFailedDependency)
		return
	case http.StatusNotFound:
		http.Error(w, "Moonfire 未找到此视频或录像", http.StatusNotFound)
		return
	default:
		http.Error(w, "Moonfire 无法处理请求", http.StatusBadGateway)
		return
	}
	if action == "inventory" {
		b.inventory(w, res.Body)
		return
	}

	expectedType := "application/json"
	if action == "view.mp4" || action == "init" {
		expectedType = "video/mp4"
	}
	contentType, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if contentType != expectedType {
		http.Error(w, "Moonfire 返回了不匹配的数据格式", http.StatusBadGateway)
		return
	}
	for _, header := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if value := res.Header.Get(header); value != "" {
			w.Header().Set(header, value)
		}
	}
	if download {
		w.Header().Set("Content-Disposition", `attachment; filename="camera-`+query.Get("s")+`.mp4"`)
	}
	w.WriteHeader(res.StatusCode)
	io.Copy(w, res.Body)
}

func (b *bridge) inventory(w http.ResponseWriter, body io.Reader) {
	var snapshot struct {
		TimeZoneName string          `json:"timeZoneName"`
		Version      string          `json:"serverVersion"`
		Cameras      json.RawMessage `json:"cameras"`
		Signals      json.RawMessage `json:"signals"`
		SignalTypes  json.RawMessage `json:"signalTypes"`
		Permissions  struct {
			ViewVideo bool `json:"viewVideo"`
		} `json:"permissions"`
	}
	if err := json.NewDecoder(io.LimitReader(body, 16<<20)).Decode(&snapshot); err != nil || snapshot.Cameras == nil {
		http.Error(w, "Moonfire 返回了无效的设备列表", http.StatusBadGateway)
		return
	}
	if !snapshot.Permissions.ViewVideo {
		http.Error(w, "Moonfire 会话需要 viewVideo 权限", http.StatusFailedDependency)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snapshot)
}

func (b *bridge) live(w http.ResponseWriter, r *http.Request) {
	target := b.requestURL(r.URL.Path, nil)
	target.Scheme = "ws"
	headers := http.Header{}
	if b.cookie != "" {
		headers.Set("Cookie", b.cookie)
	}
	conn, _, err := b.ws.DialContext(r.Context(), target.String(), headers)
	if err != nil {
		http.Error(w, "Moonfire 无法打开实时视频", http.StatusBadGateway)
		return
	}
	defer conn.Close()
	interrupt := context.AfterFunc(r.Context(), func() { conn.Close() })
	defer interrupt()
	conn.SetReadLimit(frameLimit)
	// Moonfire pings every 30 seconds, even while the camera is offline.
	conn.SetPingHandler(func(data string) error {
		conn.SetReadDeadline(time.Now().Add(80 * time.Second))
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	if err := controller.Flush(); err != nil {
		return
	}
	frame := bufio.NewWriterSize(w, 64<<10)
	for {
		conn.SetReadDeadline(time.Now().Add(80 * time.Second))
		kind, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if kind == websocket.TextMessage {
			var message struct{ Type string }
			if json.Unmarshal(data, &message) == nil && (message.Type == "error" || message.Type == "drop" || message.Type == "dropped") {
				return
			}
			continue
		}
		controller.SetWriteDeadline(time.Now().Add(15 * time.Second))
		if binary.Write(frame, binary.BigEndian, uint32(len(data))) != nil {
			return
		}
		if _, err := frame.Write(data); err != nil {
			return
		}
		if frame.Flush() != nil || controller.Flush() != nil {
			return
		}
	}
}
