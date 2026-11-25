package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestSDKSignalRelay(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test adapter uses a POSIX script")
	}
	adapter := filepath.Join(t.TempDir(), "adapter")
	code := `#!/bin/sh
set -eu
test "$#" -eq 0
IFS= read -r version
IFS= read -r host
IFS= read -r port
IFS= read -r username
IFS= read -r password
IFS= read -r channel
IFS= read -r kind
test "$version" = "JIAJING/1"
test "$host" = "192.168.1.50"
test "$port" = "8000"
test "$username" = "test-user"
test "$password" = " test password "
test "$channel" = "1"
test "$kind" = "ipc"
printf 'unstructured native log\n'
printf 'JIAJING\t1\tREADY\nJIAJING\t1\tMOTION_ON\nJIAJING\t1\tMOTION_OFF\n'
printf 'JIAJING\t1\tDISCONNECTED\n'
`
	if err := os.WriteFile(adapter, []byte(code), 0700); err != nil {
		t.Fatal(err)
	}
	var lock sync.Mutex
	var states []int
	backend := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"permissions":{"updateSignals":true},"user":{"session":{"csrf":"fixture"}}}`)
			return
		}
		var event struct {
			CSRF   string
			IDs    []int `json:"signalIds"`
			States []int
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/signals" ||
			json.NewDecoder(r.Body).Decode(&event) != nil || event.CSRF != "fixture" ||
			!slices.Equal(event.IDs, []int{4}) || len(event.States) != 1 {
			t.Error("unexpected SDK event update")
			w.WriteHeader(400)
			return
		}
		lock.Lock()
		states = append(states, event.States[0])
		lock.Unlock()
		fmt.Fprint(w, "{}")
	})
	configuration := sdkConfig{
		Host: "192.168.1.50", Port: 8000, Username: "test-user", Password: " test password ",
		Channel: 1, DeviceKind: "ipc", SignalID: 4, StillState: 1, MotionState: 2, MotionHoldSeconds: 12,
	}
	writer := &signalWriter{bridge: backend, config: motionConfig{SignalID: 4, MotionState: 2, StillState: 1}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := relaySDK(ctx, adapter, configuration, writer)
	if err == nil || err.Error() != "设备与原生 SDK 的告警连接已断开" {
		t.Fatalf("unexpected SDK relay result: %v", err)
	}
	lock.Lock()
	defer lock.Unlock()
	if !slices.Equal(states, []int{1, 2, 1}) {
		t.Fatalf("expected still/motion/still, got %v", states)
	}
}

func TestSDKLoginRejectsUnsupportedInputs(t *testing.T) {
	valid := sdkConfig{
		Host: "192.168.1.50", Port: 8000, Username: "viewer", Password: "test-only",
		Channel: 1, DeviceKind: "ipc", SignalID: 1, StillState: 1, MotionState: 2, MotionHoldSeconds: 12,
	}
	if err := valid.validate(); err != nil {
		t.Fatal(err)
	}
	publicDevice := valid
	publicDevice.Host = "8.8.8.8"
	if err := publicDevice.validate(); err == nil {
		t.Fatal("accepted a public address for a camera")
	}
	lineBreak := valid
	lineBreak.Username = "viewer\nchanged"
	if err := lineBreak.validate(); err == nil {
		t.Fatal("accepted a line break in SDK login fields")
	}
}
