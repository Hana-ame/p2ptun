package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// runtimeStats 记录运行期关键指标, 供 status 子命令与健康检查接口查询
type runtimeStats struct {
	startTime     time.Time
	mode          string
	peerID        string
	remotePeer    string
	targetOrLis   string
	currentBroker atomic.Value // string
	signalingUp   atomic.Bool
	activePeers   atomic.Int64
	activeStreams atomic.Int64
	reconnects    atomic.Uint64
	bytesTx       atomic.Uint64
	bytesRx       atomic.Uint64
	lastRTTMs     atomic.Int64
	lastError     atomic.Value // string

	mu          sync.Mutex
	triggerConn func()
}

var globalStats = &runtimeStats{
	startTime: time.Now(),
}

func (s *runtimeStats) setError(err error) {
	if err != nil {
		s.lastError.Store(fmt.Sprintf("%s (%s)", err.Error(), time.Now().Format("15:04:05")))
	}
}

func (s *runtimeStats) setTriggerReconnect(fn func()) {
	s.mu.Lock()
	s.triggerConn = fn
	s.mu.Unlock()
}

func (s *runtimeStats) triggerReconnect() {
	s.mu.Lock()
	fn := s.triggerConn
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
}

type statusSnapshot struct {
	Version       string `json:"version"`
	PID           int    `json:"pid"`
	Mode          string `json:"mode"`
	PeerID        string `json:"peer_id"`
	RemotePeer    string `json:"remote_peer,omitempty"`
	Endpoint      string `json:"endpoint"`
	Broker        string `json:"broker"`
	UptimeSec     int64  `json:"uptime_sec"`
	Uptime        string `json:"uptime"`
	SignalingUp   bool   `json:"signaling_up"`
	ActivePeers   int64  `json:"active_peers"`
	ActiveStreams int64  `json:"active_streams"`
	Reconnects    uint64 `json:"reconnects"`
	BytesTx       uint64 `json:"bytes_tx"`
	BytesRx       uint64 `json:"bytes_rx"`
	LastRTTMs     int64  `json:"last_rtt_ms"`
	DNSServers    string `json:"dns_servers"`
	LastError     string `json:"last_error,omitempty"`
}

func (s *runtimeStats) snapshot() statusSnapshot {
	broker, _ := s.currentBroker.Load().(string)
	lastErr, _ := s.lastError.Load().(string)
	up := time.Since(s.startTime).Round(time.Second)
	return statusSnapshot{
		Version:       version,
		PID:           os.Getpid(),
		Mode:          s.mode,
		PeerID:        s.peerID,
		RemotePeer:    s.remotePeer,
		Endpoint:      s.targetOrLis,
		Broker:        broker,
		UptimeSec:     int64(up.Seconds()),
		Uptime:        up.String(),
		SignalingUp:   s.signalingUp.Load(),
		ActivePeers:   s.activePeers.Load(),
		ActiveStreams: s.activeStreams.Load(),
		Reconnects:    s.reconnects.Load(),
		BytesTx:       s.bytesTx.Load(),
		BytesRx:       s.bytesRx.Load(),
		LastRTTMs:     s.lastRTTMs.Load(),
		DNSServers:    strings.Join(getActiveDNS(), ","),
		LastError:     lastErr,
	}
}

// rotatingFileWriter 实现轻量级按大小自动轮转的日志文件写入器, 防止常驻写满手机/服务器磁盘
type rotatingFileWriter struct {
	mu      sync.Mutex
	path    string
	maxSize int64
	file    *os.File
	size    int64
}

func newRotatingFileWriter(path string, maxMB int) (*rotatingFileWriter, error) {
	if maxMB <= 0 {
		maxMB = 10
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	var sz int64
	if info, err := f.Stat(); err == nil {
		sz = info.Size()
	}
	return &rotatingFileWriter{
		path:    path,
		maxSize: int64(maxMB) * 1024 * 1024,
		file:    f,
		size:    sz,
	}, nil
}

func (w *rotatingFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, fmt.Errorf("log writer closed")
	}
	if w.size+int64(len(p)) > w.maxSize {
		_ = w.file.Close()
		backup := w.path + ".1"
		_ = os.Remove(backup)
		_ = os.Rename(w.path, backup)
		f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			return 0, err
		}
		w.file = f
		w.size = 0
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		return err
	}
	return nil
}

// defaultDaemonPaths 为未显式传路径的常驻模式生成默认 pidfile 与 logfile 路径
func defaultDaemonPaths(mode string) (pidFile, logFile string) {
	baseDir := os.TempDir()
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cand := filepath.Join(home, ".p2ptun")
		if err := os.MkdirAll(cand, 0755); err == nil {
			baseDir = cand
		}
	}
	return filepath.Join(baseDir, "p2ptun-"+mode+".pid"), filepath.Join(baseDir, "p2ptun-"+mode+".log")
}

// maybeDaemonize 若开启 -daemon 且尚未处于后台子进程中, 启动后台守护进程并退出父进程
func maybeDaemonize(cfg *config) (*rotatingFileWriter, func(), error) {
	defPID, defLog := defaultDaemonPaths(cfg.mode)
	if cfg.daemon {
		if cfg.pidFile == "" {
			cfg.pidFile = defPID
		}
		if cfg.logFile == "" {
			cfg.logFile = defLog
		}
	}

	if cfg.daemon && os.Getenv("_P2PTUN_DAEMON_CHILD") != "1" {
		exe, err := os.Executable()
		if err != nil {
			exe = os.Args[0]
		}
		cmd := exec.Command(exe, os.Args[1:]...)
		cmd.Env = append(os.Environ(), "_P2PTUN_DAEMON_CHILD=1")
		if cfg.logFile != "" {
			if dir := filepath.Dir(cfg.logFile); dir != "" && dir != "." {
				_ = os.MkdirAll(dir, 0755)
			}
			f, err := os.OpenFile(cfg.logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
			if err != nil {
				return nil, nil, fmt.Errorf("open logfile %s: %w", cfg.logFile, err)
			}
			cmd.Stdout = f
			cmd.Stderr = f
			defer f.Close()
		}
		if err := cmd.Start(); err != nil {
			return nil, nil, fmt.Errorf("start background daemon: %w", err)
		}
		fmt.Printf("p2ptun %s daemon started (pid=%d, pidfile=%s, logfile=%s)\n",
			cfg.mode, cmd.Process.Pid, cfg.pidFile, cfg.logFile)
		os.Exit(0)
	}

	var logWriter *rotatingFileWriter
	if cfg.logFile != "" {
		rw, err := newRotatingFileWriter(cfg.logFile, cfg.logMaxMB)
		if err != nil {
			return nil, nil, fmt.Errorf("setup log writer: %w", err)
		}
		logWriter = rw
		if os.Getenv("_P2PTUN_DAEMON_CHILD") == "1" {
			log.SetOutput(rw)
		} else {
			log.SetOutput(io.MultiWriter(os.Stderr, rw))
		}
	}

	if cfg.pidFile != "" {
		if dir := filepath.Dir(cfg.pidFile); dir != "" && dir != "." {
			_ = os.MkdirAll(dir, 0755)
		}
		_ = os.WriteFile(cfg.pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0644)
	}

	cleanup := func() {
		if cfg.pidFile != "" {
			_ = os.Remove(cfg.pidFile)
		}
		if logWriter != nil {
			_ = logWriter.Close()
		}
	}
	return logWriter, cleanup, nil
}

// startStatusServer 启动可选的本地 HTTP 状态与控制接口 (-status-addr 127.0.0.1:9222)
func startStatusServer(addr string, stop <-chan struct{}) (net.Listener, error) {
	if addr == "" {
		return nil, nil
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		snap := globalStats.snapshot()
		w.Header().Set("Content-Type", "application/json")
		if snap.SignalingUp || snap.ActivePeers > 0 {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(snap)
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(globalStats.snapshot())
	})
	mux.HandleFunc("/reconnect", func(w http.ResponseWriter, r *http.Request) {
		globalStats.triggerReconnect()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"action":"reconnect_triggered"}` + "\n"))
	})

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
	go func() {
		<-stop
		_ = srv.Close()
	}()
	go func() {
		if err := srv.Serve(lis); err != nil && err != http.ErrServerClosed {
			log.Printf("status server error: %v", err)
		}
	}()
	log.Printf("status server listening on http://%s/status", lis.Addr().String())
	return lis, nil
}

// runStatusCmd 实现 `p2ptun status` 子命令
func runStatusCmd(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	mode := fs.String("mode", "connect", "mode to inspect (expose or connect)")
	pidFile := fs.String("pidfile", envOr("P2PTUN_PIDFILE", ""), "path to pidfile")
	statusAddr := fs.String("status-addr", envOr("P2PTUN_STATUS_ADDR", ""), "status server address")
	_ = fs.Parse(args)

	if *statusAddr != "" {
		urlStr := *statusAddr
		if !strings.HasPrefix(urlStr, "http://") && !strings.HasPrefix(urlStr, "https://") {
			urlStr = "http://" + urlStr
		}
		urlStr = strings.TrimRight(urlStr, "/") + "/status"
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(urlStr)
		if err == nil {
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			fmt.Print(string(body))
			return
		}
	}

	// 检查 pidfile 与 log
	for _, m := range []string{*mode, "expose", "connect"} {
		pf := *pidFile
		lf := ""
		if pf == "" {
			pf, lf = defaultDaemonPaths(m)
		}
		data, err := os.ReadFile(pf)
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			continue
		}
		fmt.Printf("p2ptun (%s): pid=%d (pidfile=%s)\n", m, pid, pf)
		if lf != "" {
			if logBytes, err := os.ReadFile(lf); err == nil {
				lines := strings.Split(strings.TrimSpace(string(logBytes)), "\n")
				if len(lines) > 10 {
					lines = lines[len(lines)-10:]
				}
				fmt.Println("--- recent logs ---")
				for _, ln := range lines {
					fmt.Println(ln)
				}
			}
		}
		return
	}
	fmt.Println("p2ptun: no running daemon pidfile found")
	os.Exit(1)
}

// runStopCmd 实现 `p2ptun stop` 子命令
func runStopCmd(args []string) {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	mode := fs.String("mode", "", "mode to stop (expose or connect)")
	pidFile := fs.String("pidfile", envOr("P2PTUN_PIDFILE", ""), "path to pidfile")
	_ = fs.Parse(args)

	var candidates []string
	if *pidFile != "" {
		candidates = append(candidates, *pidFile)
	} else if *mode != "" {
		pf, _ := defaultDaemonPaths(*mode)
		candidates = append(candidates, pf)
	} else {
		pf1, _ := defaultDaemonPaths("expose")
		pf2, _ := defaultDaemonPaths("connect")
		candidates = append(candidates, pf1, pf2)
	}

	stoppedAny := false
	for _, pf := range candidates {
		data, err := os.ReadFile(pf)
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			continue
		}
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Signal(os.Interrupt)
			time.Sleep(300 * time.Millisecond)
			_ = proc.Kill()
			_ = os.Remove(pf)
			fmt.Printf("p2ptun: stopped pid=%d (%s)\n", pid, pf)
			stoppedAny = true
		}
	}
	if !stoppedAny {
		fmt.Println("p2ptun: no active daemon found to stop")
	}
}

// runServiceCmd 生成 systemd 或 Termux 常驻自启脚本
func runServiceCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: p2ptun service [systemd|termux] [expose|connect]")
		os.Exit(2)
	}
	target := args[0]
	mode := "expose"
	if len(args) > 1 {
		mode = args[1]
	}
	switch target {
	case "systemd":
		if mode == "connect" {
			fmt.Print(`# /etc/systemd/system/p2ptun-connect.service
[Unit]
Description=p2ptun connect daemon (P2P TCP tunnel client)
Wants=network-online.target
After=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
EnvironmentFile=/etc/p2ptun/connect.env
ExecStart=/usr/local/bin/p2ptun connect
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=3
NoNewPrivileges=true
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
`)
		} else {
			fmt.Print(`# /etc/systemd/system/p2ptun.service
[Unit]
Description=p2ptun expose daemon (P2P TCP tunnel server)
Wants=network-online.target
After=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
EnvironmentFile=/etc/p2ptun/env
ExecStart=/usr/local/bin/p2ptun expose
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=3
NoNewPrivileges=true
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
`)
		}
	case "termux":
		fmt.Printf(`#!/data/data/com.termux/files/usr/bin/sh
# Termux 常驻守护脚本 (可放入 ~/.termux/boot/p2ptun.sh 或直接执行)
command -v termux-wake-lock >/dev/null 2>&1 && termux-wake-lock

export P2PTUN_PEER="${P2PTUN_PEER:-wslssh-a1b2}"
export P2PTUN_SECRET="${P2PTUN_SECRET:-hunter2}"
export P2PTUN_LISTEN="${P2PTUN_LISTEN:-127.0.0.1:2222}"

exec p2ptun %s -daemon
`, mode)
	default:
		fmt.Fprintf(os.Stderr, "unknown service type %q (supported: systemd, termux)\n", target)
		os.Exit(2)
	}
}
