package main

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSDKVideoAddressAndHandshake(t *testing.T) {
	if _, err := rtspPublishAddress("rtsp://127.0.0.1:8554/camera/front"); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{
		"rtsp://192.168.1.33:8554/camera", "rtsp://127.0.0.1/camera",
		"rtsp://viewer:cookie@127.0.0.1:8554/camera", "rtsp://127.0.0.1:8554/test/../bad",
	} {
		if _, err := rtspPublishAddress(address); err == nil {
			t.Fatal("accepted invalid publication address")
		}
	}
	reader := bufio.NewReader(strings.NewReader("JIAJING-VIDEO/1 h264\nframes"))
	if format, err := sdkVideoFormat(context.Background(), reader); err != nil || format != "h264" {
		t.Fatalf("video handshake was not recognized: %q %v", format, err)
	}
	remainder, _ := reader.ReadString('\n')
	if remainder != "frames" {
		t.Fatal("video bytes were lost after handshake")
	}
	args := strings.Join(ffmpegStreamArgs("h264", 12, "rtsp://127.0.0.1:8554/camera"), " ")
	if !strings.Contains(args, "-r 12 -f h264 -i pipe:0") || !strings.Contains(args, "-c:v copy") {
		t.Fatal("missing raw H.264 input timing or relay settings")
	}
}

func TestSDKVideoProcessPipe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test adapter uses POSIX scripts")
	}
	base := t.TempDir()
	adapter := filepath.Join(base, "adapter")
	converter := filepath.Join(base, "converter")
	// SDK logs and credentials are never forwarded to the video process.
	mock := `#!/bin/sh
set -eu
test "$1" = "--stream-sub"
cat >/dev/null
printf 'private library diagnostics\n' >&2
printf 'JIAJING-VIDEO/1 mpeg\nmedia payload'
`
	probe := `#!/bin/sh
set -eu
cat > "` + filepath.Join(base, "received") + `"
`
	if err := os.WriteFile(adapter, []byte(mock), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(converter, []byte(probe), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(context.Background(), 6*time.Second)
	defer stop()
	if err := relaySDKVideo(ctx, adapter, converter, sdkStreamConfig{Stream: "sub", FPS: 15},
		"rtsp://127.0.0.1:8554/test"); err == nil || ctx.Err() != nil {
		t.Fatalf("expected end of the fixture stream, got %v", err)
	}
	content, err := os.ReadFile(filepath.Join(base, "received"))
	if err != nil || string(content) != "media payload" {
		t.Fatalf("video stream was not copied intact: %v", err)
	}
}
