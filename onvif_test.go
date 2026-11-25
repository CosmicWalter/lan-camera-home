package main

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

func soapReply(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/soap+xml")
	fmt.Fprint(w, `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://www.w3.org/2005/08/addressing" xmlns:v="urn:sample:events"><s:Body>`+body+`</s:Body></s:Envelope>`)
}

func TestONVIFProbe(t *testing.T) {
	response := `<Envelope><Header><RelatesTo>urn:uuid:test</RelatesTo></Header>
<Body><ProbeMatches><ProbeMatch><Scopes>onvif://www.onvif.org/name/Front%20Door</Scopes>
<XAddrs>http://192.168.1.30/onvif/device_service http://192.168.1.32/onvif/device_service</XAddrs>
</ProbeMatch></ProbeMatches></Body></Envelope>`
	peer := netip.MustParseAddr("192.168.1.30")
	got := probeMatches([]byte(response), "urn:uuid:test", peer)
	if len(got) != 1 || got[0].Name != "Front Door" || got[0].Endpoint != "http://192.168.1.30/onvif/device_service" {
		t.Fatal("unexpected discovery result", got)
	}
	if got := probeMatches([]byte(response), "urn:uuid:other", peer); len(got) != 0 {
		t.Fatal("accepted response to different probe")
	}
}

func TestONVIFProfiles(t *testing.T) {
	camera := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content, _ := io.ReadAll(r.Body)
		var envelope struct {
			Token struct {
				Username string
				Password string
				Nonce    string
				Created  string
			} `xml:"Header>Security>UsernameToken"`
		}
		if err := xml.Unmarshal(content, &envelope); err != nil || envelope.Token.Username != "viewer<1>" {
			t.Error("malformed WS-Security username token")
		}
		nonce, err := base64.StdEncoding.DecodeString(envelope.Token.Nonce)
		hash := sha1.New()
		hash.Write(nonce)
		io.WriteString(hash, envelope.Token.Created+"test-only")
		if err != nil || len(nonce) != 16 || envelope.Token.Password != base64.StdEncoding.EncodeToString(hash.Sum(nil)) {
			t.Error("incorrect WS-Security digest")
		}

		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		switch params["action"] {
		case deviceNS + "/GetServices":
			soapReply(w, `<GetServicesResponse><Service><Namespace>`+media2NS+`</Namespace><XAddr>http://example.invalid:`+
				strings.Split(r.Host, ":")[1]+`/onvif/media2</XAddr></Service></GetServicesResponse>`)
		case media2NS + "/GetProfiles":
			if r.URL.Path != "/onvif/media2" {
				t.Error("incorrect Media2 path")
			}
			soapReply(w, `<GetProfilesResponse><Profiles token="stream&amp;1"><Name>Main</Name><Configurations><VideoEncoder>
<Encoding>H264</Encoding><Resolution><Width>1920</Width><Height>1080</Height></Resolution>
</VideoEncoder></Configurations></Profiles></GetProfilesResponse>`)
		case media2NS + "/GetStreamUri":
			if !strings.Contains(string(content), "<m:ProfileToken>stream&amp;1</m:ProfileToken>") {
				t.Error("profile token was not XML escaped")
			}
			soapReply(w, `<GetStreamUriResponse><Uri>rtsp://discard:discard@0.0.0.0:554/main</Uri></GetStreamUriResponse>`)
		case deviceNS + "/GetDeviceInformation":
			soapReply(w, `<GetDeviceInformationResponse><Manufacturer>Fixture</Manufacturer><Model>ONVIF</Model></GetDeviceInformationResponse>`)
		default:
			t.Error("unexpected SOAP action")
			w.WriteHeader(501)
		}
	}))
	defer camera.Close()

	b := fixture(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	body, _ := json.Marshal(map[string]string{"endpoint": camera.URL, "username": "viewer<1>", "password": "test-only"})
	for _, test := range []struct {
		origin string
		status int
	}{
		{"", 403}, {pagesOrigin, 403}, {"http://" + b.host, 200},
	} {
		req := httptest.NewRequest(http.MethodPost, "http://"+b.host+"/api/onvif/profiles", strings.NewReader(string(body)))
		req.Header.Set("Origin", test.origin)
		req.Header.Set("Authorization", "Bearer "+b.token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		b.ServeHTTP(rec, req)
		if rec.Code != test.status {
			t.Fatalf("ONVIF profile endpoint returned %d: %s", rec.Code, rec.Body)
		}
		if rec.Code == 200 {
			var info onvifInventory
			if json.Unmarshal(rec.Body.Bytes(), &info) != nil || info.Model != "ONVIF" || len(info.Profiles) != 1 ||
				info.Profiles[0].RTSP != "rtsp://127.0.0.1:554/main" || info.Profiles[0].Width != 1920 ||
				strings.Contains(rec.Body.String(), "discard") || strings.Contains(rec.Body.String(), "test-only") {
				t.Fatal("unexpected stream output", rec.Body.String())
			}
		}
	}
}

func TestONVIFMotionRelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var lock sync.Mutex
	var states []int
	var begins []int64
	var pulls int
	var unsubscribed bool
	camera := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		lock.Lock()
		defer lock.Unlock()
		switch params["action"] {
		case deviceNS + "/GetServices":
			soapReply(w, `<GetServicesResponse><Service><Namespace>`+eventsNS+`</Namespace><XAddr>/events</XAddr></Service></GetServicesResponse>`)
		case eventsNS + "/EventPortType/CreatePullPointSubscriptionRequest":
			soapReply(w, `<CreatePullPointSubscriptionResponse><SubscriptionReference><a:Address>/pull</a:Address>
<a:ReferenceParameters><v:ID>17</v:ID></a:ReferenceParameters>
</SubscriptionReference></CreatePullPointSubscriptionResponse>`)
		case eventsNS + "/PullPointSubscription/PullMessagesRequest":
			if r.URL.Path != "/pull" || !strings.Contains(string(body), "IsReferenceParameter") || !strings.Contains(string(body), "urn:sample:events") {
				t.Error("pull reference not preserved")
			}
			pulls++
			var output string
			if pulls == 1 {
				for _, data := range []struct{ source, state string }{{"ignored", "true"}, {"front", "true"}, {"front", "false"}} {
					output += `<NotificationMessage><Topic>tns1:VideoSource/MotionAlarm</Topic><Message>
<Message><Source><SimpleItem Name="Source" Value="` + data.source + `"/></Source>
<Data><SimpleItem Name="State" Value="` + data.state + `"/></Data></Message></Message></NotificationMessage>`
				}
			}
			soapReply(w, "<PullMessagesResponse>"+output+"</PullMessagesResponse>")
		case subManager + "UnsubscribeRequest":
			unsubscribed = true
			soapReply(w, "<UnsubscribeResponse/>")
		default:
			w.WriteHeader(501)
		}
	}))
	defer camera.Close()
	moonfire := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/" {
			fmt.Fprint(w, `{"permissions":{"updateSignals":true},"user":{"session":{"csrf":"sample-csrf"}}}`)
			return
		}
		lock.Lock()
		defer lock.Unlock()
		var payload struct {
			CSRF  string `json:"csrf"`
			IDs   []int  `json:"signalIds"`
			State []int  `json:"states"`
			Start struct {
				Base string
				Time int64 `json:"rel90k"`
			}
		}
		if r.URL.Path != "/api/signals" || r.Method != http.MethodPost ||
			json.NewDecoder(r.Body).Decode(&payload) != nil || payload.CSRF != "sample-csrf" ||
			len(payload.IDs) != 1 || payload.IDs[0] != 7 || len(payload.State) != 1 || payload.Start.Base != "epoch" {
			t.Error("unexpected Moonfire request")
			w.WriteHeader(400)
			return
		}
		states = append(states, payload.State[0])
		begins = append(begins, payload.Start.Time)
		fmt.Fprint(w, "{}")
	})
	device, err := newONVIF(camera.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	writer := &signalWriter{bridge: moonfire, config: motionConfig{SignalID: 7, MotionState: 2, StillState: 1, SourceToken: "front"}}
	done := make(chan error, 1)
	go func() { done <- relayONVIF(ctx, device, writer) }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		<-ticker.C
		lock.Lock()
		updated := len(states) >= 2
		lock.Unlock()
		if updated {
			break
		}
	}
	cancel()
	<-done
	lock.Lock()
	defer lock.Unlock()
	if len(states) != 2 || states[0] != 2 || states[1] != 1 || begins[0] >= begins[1] || !unsubscribed {
		t.Fatal("motion transitions or cleanup missing", states, begins, unsubscribed)
	}
}
