package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

const testCameraPath = "/api/cameras/aaaaaaaa-aaaa-4aaa-8aaa-000000000001/main/"

func fixture(t *testing.T, handler http.HandlerFunc) *bridge {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	b, err := newBridge("127.0.0.1:8787", strings.TrimPrefix(upstream.URL, "http://"), "")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func exchange(b *bridge, method, target, origin, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, req)
	return rec
}

func TestBridgeAccess(t *testing.T) {
	var upstreamCalls atomic.Int32
	b := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.URL.RequestURI() != "/api/?days=true" {
			t.Error("unexpected inventory URL", r.URL.RequestURI())
		}
		fmt.Fprint(w, `{"cameras":[],"timeZoneName":"UTC","user":{"session":{"csrf":"private"}},"permissions":{"viewVideo":true,"readCameraConfigs":true}}`)
	})
	for _, test := range []struct {
		name, method, target, origin string
		withToken                    bool
		status                       int
	}{
		{"no pair", "GET", "/api/", pagesOrigin, false, 401},
		{"wrong origin", "GET", "/api/", "https://example.org", true, 403},
		{"wrong host", "GET", "http://example.org/api/", "", true, 403},
		{"unexpected path", "GET", "/api/users/", pagesOrigin, true, 404},
		{"write", "POST", "/api/signals", pagesOrigin, true, 405},
		{"unexpected query", "GET", "/api/signals?cameraConfigs=true", pagesOrigin, true, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := test.target
			if strings.HasPrefix(target, "/") {
				target = "http://" + b.host + target
			}
			pairing := ""
			if test.withToken {
				pairing = b.token
			}
			if result := exchange(b, test.method, target, test.origin, pairing); result.Code != test.status {
				t.Fatalf("got %d, want %d", result.Code, test.status)
			}
		})
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("unexpected proxy requests: %d", got)
	}

	req := httptest.NewRequest("OPTIONS", "http://"+b.host+"/api/", nil)
	req.Header.Set("Origin", pagesOrigin)
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "authorization")
	req.Header.Set("Access-Control-Request-Private-Network", "true")
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, req)
	if rec.Code != 204 || rec.Header().Get("Access-Control-Allow-Origin") != pagesOrigin ||
		rec.Header().Get("Access-Control-Allow-Private-Network") != "true" {
		t.Fatal("preflight failed")
	}
	inventory := exchange(b, "GET", "http://"+b.host+"/api/?cameraConfigs=true", pagesOrigin, b.token)
	if inventory.Code != 200 || !strings.Contains(inventory.Body.String(), `"cameras":[]`) ||
		strings.Contains(inventory.Body.String(), "csrf") || strings.Contains(inventory.Body.String(), "readCameraConfigs") {
		t.Fatal("inventory was not filtered", inventory.Code, inventory.Body.String())
	}
}

func TestMediaTicketRangeAndTampering(t *testing.T) {
	b := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != testCameraPath+"view.mp4" || r.URL.RawQuery != "s=12-14%402" ||
			r.Header.Get("Range") != "bytes=0-3" {
			t.Error("unexpected media request", r.URL.RequestURI())
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 0-3/16")
		w.WriteHeader(http.StatusPartialContent)
		fmt.Fprint(w, "data")
	})
	base := "http://" + b.host
	params := url.Values{"path": {testCameraPath + "view.mp4"}, "s": {"12-14@2"}, "download": {"1"}}
	ticket := exchange(b, "GET", base+"/api/media-ticket?"+params.Encode(), pagesOrigin, b.token)
	var value struct{ URL string }
	if ticket.Code != 200 || json.Unmarshal(ticket.Body.Bytes(), &value) != nil {
		t.Fatal("failed to produce media ticket", ticket.Code)
	}

	req := httptest.NewRequest("GET", base+value.URL, nil)
	req.Header.Set("Range", "bytes=0-3")
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Header().Get("Content-Range") != "bytes 0-3/16" ||
		!strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment;") || rec.Body.String() != "data" {
		t.Fatal("media range was not forwarded", rec.Code)
	}
	tampered := strings.Replace(value.URL, "s=12", "s=42", 1)
	if rec := exchange(b, "GET", base+tampered, "", ""); rec.Code != 401 {
		t.Fatal("tampered media ticket was accepted", rec.Code)
	}
}

func TestLiveFrames(t *testing.T) {
	b := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		if err := conn.WriteMessage(websocket.BinaryMessage, []byte("frame")); err != nil {
			t.Error(err)
		}
	})
	rec := exchange(b, "GET", "http://"+b.host+testCameraPath+"live.m4s", pagesOrigin, b.token)
	bytes := rec.Body.Bytes()
	if rec.Code != 200 || len(bytes) != 9 || binary.BigEndian.Uint32(bytes[:4]) != 5 || string(bytes[4:]) != "frame" {
		t.Fatal("invalid live frame", rec.Code, len(bytes))
	}
}
