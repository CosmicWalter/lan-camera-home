package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type cameraSetup struct {
	binary string
	dir    string
}

func openCameraSetup(directory, binary string) (*cameraSetup, error) {
	if directory == "" {
		return nil, nil
	}
	if runtime.GOOS == "windows" {
		return nil, errors.New("本机 Moonfire 配置需要 Linux 或 macOS 上的 Moonfire 命令")
	}
	dir, err := filepath.Abs(directory)
	if err != nil || strings.ContainsAny(dir, "?#\n\r") {
		return nil, errors.New("Moonfire 数据库目录无效")
	}
	file, err := os.Stat(filepath.Join(dir, "db"))
	if err != nil || !file.Mode().IsRegular() {
		return nil, errors.New("请先使用 Moonfire init 初始化指定的数据库目录")
	}
	bin, err := exec.LookPath(binary)
	if err != nil {
		return nil, errors.New("找不到 Moonfire 命令，请检查 -moonfire-bin")
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, errors.New("Moonfire 配置需要 sqlite3 命令")
	}
	return &cameraSetup{binary: bin, dir: dir}, nil
}

type cameraSetupForm struct {
	Name      string
	Main      string
	Sub       string
	Username  string
	Password  string
	StorageID int64
	RetainGiB int64
}

func localRTSP(input string) (string, error) {
	if len(input) > 2048 {
		return "", errors.New("请输入有效的本地 RTSP 地址")
	}
	target, err := url.Parse(strings.TrimSpace(input))
	if err != nil || target.Scheme != "rtsp" || target.User != nil || target.Opaque != "" || target.Fragment != "" {
		return "", errors.New("请输入有效的本地 RTSP 地址，并单独填写账号密码")
	}
	ip, err := netip.ParseAddr(target.Hostname())
	if err != nil || !localIP(ip.Unmap()) || ip.Zone() != "" {
		return "", errors.New("RTSP 地址须使用局域网或本机 IP")
	}
	if port := target.Port(); port != "" {
		number, err := strconv.ParseUint(port, 10, 16)
		if err != nil || number == 0 {
			return "", errors.New("RTSP 端口无效")
		}
	}
	return target.String(), nil
}

func (form *cameraSetupForm) validate() error {
	form.Name = strings.TrimSpace(form.Name)
	if form.Name == "" || len([]rune(form.Name)) > 48 || strings.IndexFunc(form.Name, unicode.IsControl) >= 0 {
		return errors.New("摄像头名称须为 1 到 48 个字符")
	}
	var err error
	form.Main, err = localRTSP(form.Main)
	if err != nil {
		return err
	}
	form.Sub = strings.TrimSpace(form.Sub)
	if form.Sub != "" {
		form.Sub, err = localRTSP(form.Sub)
		if err != nil {
			return err
		}
		if form.Main == form.Sub {
			return errors.New("主码流和子码流需要不同的 RTSP 地址")
		}
	}
	if len(form.Username) > 256 || len(form.Password) > 512 || strings.ContainsAny(form.Username+form.Password, "\x00\n\r") ||
		(form.Username == "" && form.Password != "") {
		return errors.New("RTSP 账号或密码格式无效")
	}
	if form.StorageID < 1 || form.RetainGiB < 1 || form.RetainGiB > 65536 {
		return errors.New("请选择录像目录并填写每路录像容量（GiB）")
	}
	return nil
}

// The CLI takes Moonfire's own exclusive directory lock before running SQLite.
func (s *cameraSetup) query(ctx context.Context, sql string) ([]byte, error) {
	run := exec.CommandContext(ctx, s.binary, "sql", "--db-dir="+s.dir, "--", "-batch", "-bail", "-init", os.DevNull, "-json")
	run.Stdin = strings.NewReader(sql)
	var diagnostics bytes.Buffer
	run.Stderr = &diagnostics
	output, err := run.Output()
	if err == nil {
		return output, nil
	}
	switch detail := diagnostics.String(); {
	case strings.Contains(detail, "unable to get exclusive lock"):
		return nil, errMoonfireRunning
	case strings.Contains(detail, "jiajing_schema_7"):
		return nil, errors.New("此 Moonfire 数据库版本不支持快捷配置")
	case strings.Contains(detail, "jiajing_camera_name"):
		return nil, errDuplicateCamera
	case strings.Contains(detail, "jiajing_storage"):
		return nil, errors.New("录像目录不存在，请重新读取目录")
	default:
		return nil, errors.New("无法读取或写入 Moonfire 配置，请检查目录与执行权限")
	}
}

var (
	errMoonfireRunning = errors.New("Moonfire 正在使用数据库。请停止录像服务后重试，桥接程序保持运行。")
	errDuplicateCamera = errors.New("该摄像头名称已存在。可重启 Moonfire 后刷新设备列表，或使用其他名称。")
)

const schemaGuard = `
CREATE TEMP TABLE jiajing_schema (version INTEGER NOT NULL CONSTRAINT jiajing_schema_7 CHECK(version = 7));
INSERT INTO jiajing_schema SELECT coalesce(max(id),0) FROM version;
`

type recordingFolder struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func (s *cameraSetup) folders(ctx context.Context) ([]recordingFolder, error) {
	output, err := s.query(ctx, schemaGuard+
		"SELECT id, json_extract(config, '$.path') AS name FROM sample_file_dir ORDER BY id;")
	if err != nil {
		return nil, err
	}
	items := make([]recordingFolder, 0)
	if len(bytes.TrimSpace(output)) != 0 && json.Unmarshal(output, &items) != nil {
		return nil, errors.New("无法读取录像目录")
	}
	for i := range items {
		if name := filepath.Base(items[i].Name); name != "." && name != string(os.PathSeparator) {
			items[i].Name = name
		} else {
			items[i].Name = "录像目录"
		}
	}
	return items, nil
}

// Hex-encoding text in the SQLite script keeps it a literal even when a name
// or password contains quotes. The script is sent through stdin.
func sqliteText(text string) string {
	return "CAST(X'" + hex.EncodeToString([]byte(text)) + "' AS TEXT)"
}

func (s *cameraSetup) create(ctx context.Context, form cameraSetupForm) error {
	config, _ := json.Marshal(struct {
		Username string `json:"username,omitempty"`
		Password string `json:"password,omitempty"`
	}{form.Username, form.Password})
	identifier, err := onvifMessageID()
	if err != nil {
		return errors.New("无法生成摄像头标识")
	}
	identifier = strings.ReplaceAll(strings.TrimPrefix(identifier, "urn:uuid:"), "-", "")
	var sql strings.Builder
	sql.WriteString("BEGIN IMMEDIATE;\n" + schemaGuard)
	fmt.Fprintf(&sql, `
CREATE TEMP TABLE jiajing_checks (
  storage_ok INTEGER CONSTRAINT jiajing_storage CHECK(storage_ok = 1),
  name_ok INTEGER CONSTRAINT jiajing_camera_name CHECK(name_ok = 1)
);
INSERT INTO jiajing_checks VALUES (
  EXISTS(SELECT 1 FROM sample_file_dir WHERE id = %d),
  NOT EXISTS(SELECT 1 FROM camera WHERE short_name = %s COLLATE NOCASE)
);
INSERT INTO camera (uuid,short_name,config) VALUES (X'%s',%s,%s);
`, form.StorageID, sqliteText(form.Name), identifier, sqliteText(form.Name), sqliteText(string(config)))
	for _, stream := range []struct{ kind, address string }{{"main", form.Main}, {"sub", form.Sub}} {
		if stream.address == "" {
			continue
		}
		config, _ := json.Marshal(struct {
			Mode      string `json:"mode"`
			URL       string `json:"url"`
			Transport string `json:"rtspTransport"`
			Bytes     int64  `json:"retainBytes"`
			Flush     int    `json:"flushIfSec"`
		}{"record", stream.address, "tcp", form.RetainGiB << 30, 120})
		fmt.Fprintf(&sql, `
INSERT INTO stream (camera_id,sample_file_dir_id,type,config,cum_recordings,cum_media_duration_90k,cum_runs)
SELECT id,%d,'%s',%s,0,0,0 FROM camera WHERE uuid = X'%s';
`, form.StorageID, stream.kind, sqliteText(string(config)), identifier)
	}
	sql.WriteString("COMMIT;\n")
	_, err = s.query(ctx, sql.String())
	return err
}

func (b *bridge) serveCameraSetup(w http.ResponseWriter, r *http.Request, origin, localOrigin string) {
	if origin != localOrigin {
		http.Error(w, "请在本机控制台配置 Moonfire", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+b.token)) != 1 {
		http.Error(w, "配对码无效", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/api/setup/info" {
		json.NewEncoder(w).Encode(map[string]bool{"enabled": b.setup != nil})
		return
	}
	if b.setup == nil {
		http.Error(w, "请用 -setup-db 启用快捷配置", http.StatusFailedDependency)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	var err error
	switch r.URL.Path {
	case "/api/setup/storage":
		var options []recordingFolder
		options, err = b.setup.folders(ctx)
		if err == nil {
			json.NewEncoder(w).Encode(options)
		}
	case "/api/setup/cameras":
		var form cameraSetupForm
		mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
		decoder.DisallowUnknownFields()
		if mediaType != "application/json" || decoder.Decode(&form) != nil {
			http.Error(w, "摄像头配置格式无效", http.StatusBadRequest)
			return
		}
		if err := form.validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		err = b.setup.create(ctx, form)
		if err == nil {
			json.NewEncoder(w).Encode(map[string]bool{"saved": true})
		}
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		code := http.StatusFailedDependency
		if errors.Is(err, errMoonfireRunning) || errors.Is(err, errDuplicateCamera) {
			code = http.StatusConflict
		}
		http.Error(w, err.Error(), code)
	}
}
