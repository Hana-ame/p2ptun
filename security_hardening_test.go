package main

// 安全回归测试: 明文密钥不得落入日志/进程列表, 状态控制接口不得被跨站触发.
// 对应审计发现: main.go:740 (日志明文密钥) 与 daemon.go /reconnect (CSRF DoS).

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScrubSecretArgs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
		leak string
	}{
		{
			name: "separate flag",
			in:   []string{"expose", "-id", "x", "-secret", "hunter2", "-daemon"},
			want: []string{"expose", "-id", "x", "-daemon"},
			leak: "hunter2",
		},
		{
			name: "equals form",
			in:   []string{"connect", "-secret=hunter2", "-listen", "127.0.0.1:2222"},
			want: []string{"connect", "-listen", "127.0.0.1:2222"},
			leak: "hunter2",
		},
		{
			name: "double dash separate",
			in:   []string{"expose", "--secret", "hunter2"},
			want: []string{"expose"},
			leak: "hunter2",
		},
		{
			name: "double dash equals",
			in:   []string{"expose", "--secret=hunter2", "-id", "x"},
			want: []string{"expose", "-id", "x"},
			leak: "hunter2",
		},
		{
			name: "no secret flag",
			in:   []string{"expose", "-id", "x"},
			want: []string{"expose", "-id", "x"},
		},
	}
	for _, tc := range cases {
		got := scrubSecretArgs(tc.in)
		if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
		if tc.leak != "" && strings.Contains(strings.Join(got, " "), tc.leak) {
			t.Fatalf("%s: secret still present in child argv: %q", tc.name, got)
		}
	}
}

func TestDaemonChildEnvCarriesSecretOnce(t *testing.T) {
	t.Setenv("P2PTUN_SECRET", "from-parent-env")
	env := daemonChildEnv("actual-secret")

	secretCount := 0
	childFlag := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "P2PTUN_SECRET=") {
			secretCount++
			if kv != "P2PTUN_SECRET=actual-secret" {
				t.Fatalf("unexpected secret entry %q", kv)
			}
		}
		if kv == "_P2PTUN_DAEMON_CHILD=1" {
			childFlag = true
		}
	}
	if secretCount != 1 {
		t.Fatalf("P2PTUN_SECRET entries = %d, want exactly 1", secretCount)
	}
	if !childFlag {
		t.Fatal("_P2PTUN_DAEMON_CHILD=1 missing from child env")
	}
}

func TestLogFileAndDaemonDirArePrivate(t *testing.T) {
	// 默认 ~/.p2ptun 目录: 新建 0700, 旧版本遗留的 0755 也要被收紧.
	home := t.TempDir()
	daemonDir := filepath.Join(home, ".p2ptun")
	if err := os.MkdirAll(daemonDir, 0o755); err != nil {
		t.Fatalf("prepare legacy daemon dir: %v", err)
	}
	if err := os.Chmod(daemonDir, 0o755); err != nil {
		t.Fatalf("chmod legacy daemon dir: %v", err)
	}
	t.Setenv("HOME", home)

	pidPath, logPath := defaultDaemonPaths("connect")
	if filepath.Dir(pidPath) != daemonDir || filepath.Dir(logPath) != daemonDir {
		t.Fatalf("default paths not under %s: pid=%s log=%s", daemonDir, pidPath, logPath)
	}
	assertPerm(t, daemonDir, 0o700)

	// 日志文件: 新建 0600, 历史 0644 的旧文件打开时收紧.
	logDir := filepath.Join(t.TempDir(), "logs")
	rw, err := newRotatingFileWriter(filepath.Join(logDir, "p2ptun.log"), 1)
	if err != nil {
		t.Fatalf("newRotatingFileWriter: %v", err)
	}
	defer rw.Close()
	logFile := filepath.Join(logDir, "p2ptun.log")
	assertPerm(t, logFile, 0o600)
	assertPerm(t, logDir, 0o700)

	legacy := filepath.Join(t.TempDir(), "legacy-0644.log")
	if err := os.WriteFile(legacy, []byte("old secret material\n"), 0o644); err != nil {
		t.Fatalf("write legacy log: %v", err)
	}
	if err := os.Chmod(legacy, 0o644); err != nil {
		t.Fatalf("chmod legacy log: %v", err)
	}
	rwLegacy, err := newRotatingFileWriter(legacy, 1)
	if err != nil {
		t.Fatalf("open legacy log: %v", err)
	}
	defer rwLegacy.Close()
	assertPerm(t, legacy, 0o600)

	// 轮转后的备份同样必须是 0600.
	rw.maxSize = 16
	if _, err := rw.Write([]byte(strings.Repeat("A", 64))); err != nil {
		t.Fatalf("rotate log: %v", err)
	}
	assertPerm(t, logFile+".1", 0o600)
	assertPerm(t, logFile, 0o600)
}

func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

func TestReconnectRequiresPostAndSameOrigin(t *testing.T) {
	stop := make(chan struct{})
	defer close(stop)
	lis, err := startStatusServer("127.0.0.1:0", stop, nil)
	if err != nil {
		t.Fatalf("startStatusServer: %v", err)
	}
	base := "http://" + lis.Addr().String() + "/reconnect"

	do := func(method, origin, secFetchSite string) int {
		t.Helper()
		req, err := http.NewRequest(method, base, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if secFetchSite != "" {
			req.Header.Set("Sec-Fetch-Site", secFetchSite)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, base, err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// 浏览器 <img src> / 地址栏: GET 必须拒绝 —— 这正是原 CSRF DoS 的触发方式.
	if got := do(http.MethodGet, "", ""); got != http.StatusMethodNotAllowed {
		t.Fatalf("GET /reconnect = %d, want 405", got)
	}
	// 本机脚本/curl 的普通 POST: 放行.
	if got := do(http.MethodPost, "", ""); got != http.StatusOK {
		t.Fatalf("POST /reconnect = %d, want 200", got)
	}
	// 浏览器跨站表单 POST 会带 Origin: 拒绝.
	if got := do(http.MethodPost, "http://evil.example", ""); got != http.StatusForbidden {
		t.Fatalf("cross-origin POST /reconnect = %d, want 403", got)
	}
	if got := do(http.MethodPost, "", "cross-site"); got != http.StatusForbidden {
		t.Fatalf("cross-site POST /reconnect = %d, want 403", got)
	}
	// 同源页面 POST: 放行.
	if got := do(http.MethodPost, base[:strings.LastIndex(base, "/")], "same-origin"); got != http.StatusOK {
		t.Fatalf("same-origin POST /reconnect = %d, want 200", got)
	}
}
