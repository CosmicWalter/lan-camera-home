package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCameraSetupLocalOnly(t *testing.T) {
	b := fixture(t, nil)
	for _, test := range []struct {
		origin string
		auth   bool
		want   int
	}{
		{pagesOrigin, true, 403}, {"", true, 403}, {"http://" + b.host, false, 401},
		{"http://" + b.host, true, 200},
	} {
		req := httptest.NewRequest("POST", "http://"+b.host+"/api/setup/info", nil)
		req.Header.Set("Origin", test.origin)
		if test.auth {
			req.Header.Set("Authorization", "Bearer "+b.token)
		}
		rec := httptest.NewRecorder()
		b.ServeHTTP(rec, req)
		if rec.Code != test.want || rec.Code == 200 && !strings.Contains(rec.Body.String(), `"enabled":false`) {
			t.Fatalf("unexpected setup authorization status %d", rec.Code)
		}
	}
}

func TestCameraSetupAtomicSQL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SQLite fixture uses a POSIX shell")
	}
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 CLI unavailable")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "db")
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, sqlite, db)
	cmd.Stdin = strings.NewReader(`
CREATE TABLE version(id INTEGER PRIMARY KEY);
INSERT INTO version VALUES (7);
CREATE TABLE sample_file_dir(id INTEGER PRIMARY KEY, config TEXT);
INSERT INTO sample_file_dir VALUES (1,'{"path":"/example/recordings"}');
CREATE TABLE camera(id INTEGER PRIMARY KEY, uuid BLOB UNIQUE, short_name TEXT, config TEXT);
CREATE TABLE stream(id INTEGER PRIMARY KEY,camera_id INTEGER,sample_file_dir_id INTEGER,type TEXT,
config TEXT,cum_recordings INTEGER,cum_media_duration_90k INTEGER,cum_runs INTEGER);
`)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}

	shim := filepath.Join(dir, "moonfire")
	code := "#!/bin/sh\nexec sqlite3 \"" + db + "\" -batch -bail -init /dev/null -json\n"
	if err := os.WriteFile(shim, []byte(code), 0700); err != nil {
		t.Fatal(err)
	}
	setup := &cameraSetup{binary: shim, dir: dir}
	folders, err := setup.folders(ctx)
	if err != nil || len(folders) != 1 || folders[0].Name != "recordings" {
		t.Fatalf("unexpected directories: %v %v", folders, err)
	}
	form := cameraSetupForm{Name: "前门 ' 一楼", Main: "rtsp://192.168.1.12/live/main", Sub: "rtsp://192.168.1.12/live/sub",
		Username: "user", Password: "password ' ) ;", StorageID: 2, RetainGiB: 5}
	if err := form.validate(); err != nil {
		t.Fatal(err)
	}
	if err := setup.create(ctx, form); err == nil {
		t.Fatal("created camera in missing storage")
	}
	form.StorageID = 1
	if err := setup.create(ctx, form); err != nil {
		t.Fatal(err)
	}
	if err := setup.create(ctx, form); !errors.Is(err, errDuplicateCamera) {
		t.Fatal("did not reject repeated submission", err)
	}
	out, err := exec.CommandContext(ctx, sqlite, db, "SELECT count(*) FROM camera; SELECT count(*) FROM stream WHERE json_extract(config,'$.retainBytes')=5368709120;").Output()
	if err != nil || string(out) != "1\n2\n" {
		t.Fatal("unexpected camera and stream rows", string(out), err)
	}
	form.Main = "rtsp://user:password@192.168.1.15/live"
	if err := form.validate(); err == nil {
		t.Fatal("accepted credentials in URL")
	}
	if err := exec.CommandContext(ctx, sqlite, db, "INSERT INTO version VALUES (8);").Run(); err != nil {
		t.Fatal(err)
	}
	form.Name, form.Main = "另一路", "rtsp://192.168.1.15/live"
	if err := setup.create(ctx, form); err == nil || !strings.Contains(err.Error(), "数据库版本") {
		t.Fatal("did not reject unsupported database version", err)
	}
}
