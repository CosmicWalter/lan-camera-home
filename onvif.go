package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/icholy/digest"
	"golang.org/x/net/ipv4"
)

const (
	deviceNS = "http://www.onvif.org/ver10/device/wsdl"
	mediaNS  = "http://www.onvif.org/ver10/media/wsdl"
	media2NS = "http://www.onvif.org/ver20/media/wsdl"
	eventsNS = "http://www.onvif.org/ver10/events/wsdl"
)

type discoveredCamera struct {
	Endpoint string `json:"endpoint"`
	Name     string `json:"name"`
}

type onvifProfile struct {
	Token    string  `xml:"token,attr" json:"-"`
	Name     string  `xml:"Name" json:"name"`
	Encoder  encoder `xml:"VideoEncoderConfiguration" json:"-"`
	Encoder2 encoder `xml:"Configurations>VideoEncoder" json:"-"`
	Encoding string  `xml:"-" json:"encoding"`
	Width    int     `xml:"-" json:"width"`
	Height   int     `xml:"-" json:"height"`
	RTSP     string  `xml:"-" json:"rtsp"`
}

type encoder struct {
	Encoding string `xml:"Encoding"`
	Width    int    `xml:"Resolution>Width"`
	Height   int    `xml:"Resolution>Height"`
}

type onvifInventory struct {
	Manufacturer string         `xml:"Manufacturer" json:"manufacturer"`
	Model        string         `xml:"Model" json:"model"`
	Profiles     []onvifProfile `xml:"-" json:"profiles"`
}

type onvifDevice struct {
	endpoint *url.URL
	client   *http.Client
	username string
	password string
}

func localIP(ip netip.Addr) bool {
	return ip.IsValid() && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast())
}

func cameraEndpoint(address string) (*url.URL, error) {
	address = strings.TrimSpace(address)
	if !strings.Contains(address, "://") {
		address = "http://" + address
	}
	target, err := url.Parse(address)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.User != nil ||
		target.Fragment != "" || target.Opaque != "" {
		return nil, errors.New("请输入 ONVIF 设备的本地 IP 地址或 HTTP(S) 设备服务地址")
	}
	ip, err := netip.ParseAddr(target.Hostname())
	if err != nil || !localIP(ip.Unmap()) || ip.Zone() != "" {
		return nil, errors.New("ONVIF 设备必须使用本地 IP 地址")
	}
	if target.Path == "" || target.Path == "/" {
		target.Path, target.RawPath = "/onvif/device_service", ""
	}
	return target, nil
}

// Some devices advertise an internal hostname or 0.0.0.0. Bind service
// requests and credentials to the IP the user selected.
func cameraService(device *url.URL, advertised string, media bool) (*url.URL, error) {
	raw, err := url.Parse(strings.TrimSpace(advertised))
	if err != nil || raw.Opaque != "" {
		return nil, errors.New("设备返回了无效的服务地址")
	}
	if !raw.IsAbs() {
		raw = device.ResolveReference(raw)
	}
	if media {
		if raw.Scheme != "rtsp" {
			return nil, errors.New("该配置未提供 RTSP 地址")
		}
	} else if raw.Scheme != "https" && !(device.Scheme == "http" && raw.Scheme == "http") {
		return nil, errors.New("设备返回了不受支持的服务协议")
	}
	host := device.Hostname()
	if port := raw.Port(); port != "" {
		raw.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		raw.Host = "[" + host + "]"
	} else {
		raw.Host = host
	}
	raw.User, raw.Fragment = nil, ""
	return raw, nil
}

func discoverONVIF(ctx context.Context) ([]discoveredCamera, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, errors.New("无法读取本机网络接口")
	}
	results := make(chan []discoveredCamera, len(interfaces))
	active := 0
	for _, iface := range interfaces {
		if iface.Flags&(net.FlagUp|net.FlagMulticast) != net.FlagUp|net.FlagMulticast || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err != nil || !prefix.Addr().Is4() {
				continue
			}
			active++
			go func(iface net.Interface, ip netip.Addr) {
				results <- probeONVIF(ctx, &iface, ip)
			}(iface, prefix.Addr())
			break
		}
	}
	if active == 0 {
		return nil, errors.New("本机没有可用于 ONVIF 发现的 IPv4 网络接口")
	}
	devices := make(map[string]discoveredCamera)
	for i := 0; i < active; i++ {
		for _, camera := range <-results {
			devices[camera.Endpoint] = camera
		}
	}
	items := make([]discoveredCamera, 0, len(devices))
	for _, item := range devices {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Endpoint < items[j].Endpoint })
	return items, ctx.Err()
}

func probeONVIF(ctx context.Context, iface *net.Interface, ip netip.Addr) []discoveredCamera {
	socket, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip, 0)))
	if err != nil {
		return nil
	}
	defer socket.Close()
	stop := context.AfterFunc(ctx, func() { socket.Close() })
	defer stop()
	socket.SetDeadline(time.Now().Add(2300 * time.Millisecond))
	packet := ipv4.NewPacketConn(socket)
	if packet.SetMulticastInterface(iface) != nil || packet.SetMulticastTTL(1) != nil {
		return nil
	}
	id, err := onvifMessageID()
	if err != nil {
		return nil
	}
	message := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery" xmlns:dn="http://www.onvif.org/ver10/network/wsdl">
<s:Header><a:MessageID>` + id + `</a:MessageID><a:To>urn:schemas-xmlsoap-org:ws:2005:04:discovery</a:To><a:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</a:Action></s:Header>
<s:Body><d:Probe><d:Types>dn:NetworkVideoTransmitter</d:Types></d:Probe></s:Body></s:Envelope>`
	destination := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 3702}
	if _, err := socket.WriteToUDP([]byte(message), destination); err != nil {
		return nil
	}
	var cameras []discoveredCamera
	data := make([]byte, 16<<10)
	for i := 0; i < 150; i++ {
		n, peer, err := socket.ReadFromUDPAddrPort(data)
		if err != nil {
			break
		}
		cameras = append(cameras, probeMatches(data[:n], id, peer.Addr().Unmap())...)
	}
	return cameras
}

func onvifMessageID() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	nonce[6] = 0x40 | (nonce[6] & 0x0f)
	nonce[8] = 0x80 | (nonce[8] & 0x3f)
	return fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", nonce[:4], nonce[4:6], nonce[6:8], nonce[8:10], nonce[10:]), nil
}

func probeMatches(data []byte, id string, peer netip.Addr) []discoveredCamera {
	if !localIP(peer) {
		return nil
	}
	var response struct {
		ID      string `xml:"Header>RelatesTo"`
		Matches []struct {
			Addresses string `xml:"XAddrs"`
			Scopes    string `xml:"Scopes"`
		} `xml:"Body>ProbeMatches>ProbeMatch"`
	}
	if xml.Unmarshal(data, &response) != nil || strings.TrimSpace(response.ID) != id {
		return nil
	}
	var cameras []discoveredCamera
	for _, match := range response.Matches {
		var name string
		for _, scope := range strings.Fields(match.Scopes) {
			if value, ok := strings.CutPrefix(scope, "onvif://www.onvif.org/name/"); ok {
				name, _ = url.PathUnescape(value)
				break
			}
		}
		for _, address := range strings.Fields(match.Addresses) {
			target, err := cameraEndpoint(address)
			if err != nil || target.Hostname() != peer.String() {
				continue
			}
			cameras = append(cameras, discoveredCamera{Endpoint: target.String(), Name: name})
		}
	}
	return cameras
}

func newONVIF(address, username, password string) (*onvifDevice, error) {
	endpoint, err := cameraEndpoint(address)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 4 * time.Second}).DialContext,
		ResponseHeaderTimeout: 6 * time.Second,
		IdleConnTimeout:       15 * time.Second,
	}
	return &onvifDevice{
		endpoint: endpoint, username: username, password: password,
		client: &http.Client{
			Transport:     &digest.Transport{Username: username, Password: password, Transport: transport},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func xmlText(value string) string {
	var encoded bytes.Buffer
	xml.EscapeText(&encoded, []byte(value))
	return encoded.String()
}

func (d *onvifDevice) soap(ctx context.Context, endpoint *url.URL, namespace, action, body string) ([]byte, error) {
	return d.soapRequest(ctx, endpoint, namespace, action, namespace+"/"+action, body, "")
}

func (d *onvifDevice) soapRequest(ctx context.Context, endpoint *url.URL, namespace, action, uri, body, headers string) ([]byte, error) {
	var security string
	if d.username != "" {
		nonce := make([]byte, 16)
		if _, err := rand.Read(nonce); err != nil {
			return nil, errors.New("无法生成 ONVIF 认证随机数")
		}
		created := time.Now().UTC().Format("2006-01-02T15:04:05Z")
		// ONVIF UsernameToken PasswordDigest uses SHA-1 over these three fields.
		hash := sha1.New()
		hash.Write(nonce)
		io.WriteString(hash, created)
		io.WriteString(hash, d.password)
		security = `<wsse:Security s:mustUnderstand="1"><wsse:UsernameToken><wsse:Username>` + xmlText(d.username) +
			`</wsse:Username><wsse:Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest">` +
			base64.StdEncoding.EncodeToString(hash.Sum(nil)) + `</wsse:Password><wsse:Nonce EncodingType="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary">` +
			base64.StdEncoding.EncodeToString(nonce) + `</wsse:Nonce><wsu:Created>` + created + `</wsu:Created></wsse:UsernameToken></wsse:Security>`
	}
	message := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:m="` + namespace + `" xmlns:wsa="http://www.w3.org/2005/08/addressing" xmlns:tt="http://www.onvif.org/ver10/schema" xmlns:wsse="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd" xmlns:wsu="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd"><s:Header>` + headers + security + `</s:Header><s:Body>` + body + `</s:Body></s:Envelope>`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(message))
	if err != nil {
		return nil, errors.New("无法创建 ONVIF 请求")
	}
	req.Header.Set("Content-Type", mime.FormatMediaType("application/soap+xml", map[string]string{"charset": "utf-8", "action": uri}))
	res, err := d.client.Do(req)
	if err != nil {
		return nil, errors.New("无法连接 ONVIF 设备，请检查地址、证书和网络")
	}
	defer res.Body.Close()
	if res.StatusCode == 401 || res.StatusCode == 403 {
		return nil, errors.New("设备拒绝认证，请检查 ONVIF 账号、密码和设备时间")
	}
	const limit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil || len(data) > limit {
		return nil, errors.New("ONVIF 响应读取失败")
	}
	var response struct {
		Body struct {
			Fault *struct {
				Value string `xml:"Code>Subcode>Value"`
			} `xml:"Fault"`
			XML string `xml:",innerxml"`
		} `xml:"Body"`
	}
	if xml.Unmarshal(data, &response) != nil || response.Body.XML == "" {
		return nil, errors.New("设备返回了无效的 ONVIF 响应")
	}
	if response.Body.Fault != nil && strings.HasSuffix(response.Body.Fault.Value, ":NotAuthorized") {
		return nil, errors.New("设备拒绝认证，请检查 ONVIF 账号、密码和设备时间")
	}
	if response.Body.Fault != nil || res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("设备无法处理 ONVIF %s 请求", action)
	}
	return data, nil
}

type onvifService struct {
	Namespace string `xml:"Namespace"`
	Address   string `xml:"XAddr"`
}

func (d *onvifDevice) services(ctx context.Context, expected ...string) ([]onvifService, error) {
	var listed struct {
		Services []onvifService `xml:"Body>GetServicesResponse>Service"`
	}
	data, err := d.soap(ctx, d.endpoint, deviceNS, "GetServices", `<m:GetServices><m:IncludeCapability>false</m:IncludeCapability></m:GetServices>`)
	if err == nil {
		xml.Unmarshal(data, &listed)
	}
	for _, service := range listed.Services {
		if service.Address != "" && slices.Contains(expected, service.Namespace) {
			return listed.Services, nil
		}
	}
	var caps struct {
		Media  string `xml:"Body>GetCapabilitiesResponse>Capabilities>Media>XAddr"`
		Events string `xml:"Body>GetCapabilitiesResponse>Capabilities>Events>XAddr"`
	}
	data, err = d.soap(ctx, d.endpoint, deviceNS, "GetCapabilities", `<m:GetCapabilities><m:Category>All</m:Category></m:GetCapabilities>`)
	if err != nil {
		return nil, err
	}
	if xml.Unmarshal(data, &caps) == nil {
		if caps.Media != "" {
			listed.Services = append(listed.Services, onvifService{Namespace: mediaNS, Address: caps.Media})
		}
		if caps.Events != "" {
			listed.Services = append(listed.Services, onvifService{Namespace: eventsNS, Address: caps.Events})
		}
	}
	return listed.Services, nil
}

func (d *onvifDevice) profiles(ctx context.Context) (onvifInventory, error) {
	services, err := d.services(ctx, mediaNS, media2NS)
	if err != nil {
		return onvifInventory{}, err
	}
	var lastErr error
	for _, kind := range []string{mediaNS, media2NS} {
		for _, service := range services {
			if service.Namespace != kind || service.Address == "" {
				continue
			}
			profiles, err := d.mediaProfiles(ctx, service)
			if err != nil {
				lastErr = err
				continue
			}
			var info struct {
				Result onvifInventory `xml:"Body>GetDeviceInformationResponse"`
			}
			if data, err := d.soap(ctx, d.endpoint, deviceNS, "GetDeviceInformation", "<m:GetDeviceInformation/>"); err == nil {
				xml.Unmarshal(data, &info)
			}
			info.Result.Profiles = profiles
			return info.Result, nil
		}
	}
	if lastErr != nil {
		return onvifInventory{}, lastErr
	}
	return onvifInventory{}, errors.New("设备未提供 ONVIF Media 或 Media2 服务")
}

func (d *onvifDevice) mediaProfiles(ctx context.Context, service onvifService) ([]onvifProfile, error) {
	endpoint, err := cameraService(d.endpoint, service.Address, false)
	if err != nil {
		return nil, err
	}
	body := "<m:GetProfiles/>"
	if service.Namespace == media2NS {
		body = "<m:GetProfiles><m:Type>VideoEncoder</m:Type></m:GetProfiles>"
	}
	data, err := d.soap(ctx, endpoint, service.Namespace, "GetProfiles", body)
	if err != nil {
		return nil, err
	}
	var response struct {
		Profiles []onvifProfile `xml:"Body>GetProfilesResponse>Profiles"`
	}
	if xml.Unmarshal(data, &response) != nil || len(response.Profiles) == 0 {
		return nil, errors.New("设备没有可读取的码流配置")
	}
	var items []onvifProfile
	for _, profile := range response.Profiles[:min(len(response.Profiles), 16)] {
		if profile.Token == "" {
			continue
		}
		streamSetup := "<m:StreamSetup><tt:Stream>RTP-Unicast</tt:Stream><tt:Transport><tt:Protocol>RTSP</tt:Protocol></tt:Transport></m:StreamSetup>"
		if service.Namespace == media2NS {
			streamSetup = "<m:Protocol>RTSP</m:Protocol>"
			profile.Encoder = profile.Encoder2
		}
		profile.Encoding, profile.Width, profile.Height = profile.Encoder.Encoding, profile.Encoder.Width, profile.Encoder.Height
		body = "<m:GetStreamUri>" + streamSetup + "<m:ProfileToken>" + xmlText(profile.Token) + "</m:ProfileToken></m:GetStreamUri>"
		if data, err := d.soap(ctx, endpoint, service.Namespace, "GetStreamUri", body); err == nil {
			var result struct {
				URI   string `xml:"Body>GetStreamUriResponse>Uri"`
				Media string `xml:"Body>GetStreamUriResponse>MediaUri>Uri"`
			}
			if xml.Unmarshal(data, &result) == nil {
				if result.URI == "" {
					result.URI = result.Media
				}
				if address, err := cameraService(d.endpoint, result.URI, true); err == nil {
					profile.RTSP = address.String()
				}
			}
		}
		items = append(items, profile)
	}
	for _, profile := range items {
		if profile.RTSP != "" {
			return items, nil
		}
	}
	return nil, errors.New("设备未返回可用的 RTSP 码流地址")
}
