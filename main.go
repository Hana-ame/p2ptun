package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"
)

const (
	version = "0.2.1"

	defaultBroker = "wss://0.peerjs.com,wss://1.peerjs.com"
	defaultKey    = "peerjs"
	defaultSTUN   = "stun:stun.cloudflare.com:3478,stun:stun.l.google.com:19302,stun:stun.miwifi.com:3478,stun:stun.qq.com:3478,stun:stun.chat.bilibili.com:3478"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	applyNetFix() // Termux 无 resolv.conf / CA 路径修复 / 真实 DNS 探测 (netfix.go)

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "expose":
		log.Printf("p2ptun %s started (DNS: %s)", version, strings.Join(getActiveDNS(), ", "))
		runExpose(os.Args[2:])
	case "connect":
		log.Printf("p2ptun %s started (DNS: %s)", version, strings.Join(getActiveDNS(), ", "))
		runConnect(os.Args[2:])
	case "status":
		runStatusCmd(os.Args[2:])
	case "stop":
		runStopCmd(os.Args[2:])
	case "service":
		runServiceCmd(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("p2ptun " + version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `p2ptun %s - P2P TCP tunnel over PeerJS signaling + WebRTC DataChannel

expose   (run where the TCP service lives, e.g. WSL / Linux server):
  p2ptun expose -secret SECRET [-id MYID] [-target 127.0.0.1:22] [-daemon]

connect  (run on the remote client side, e.g. Termux / laptop):
  p2ptun connect -peer REMOTE_ID -secret SECRET [-listen 127.0.0.1:2222] [-daemon]

daemon & management commands:
  p2ptun status  [-mode expose|connect] [-status-addr 127.0.0.1:18222]
  p2ptun stop    [-mode expose|connect] [-pidfile PATH]
  p2ptun service [systemd|termux] [expose|connect]

common flags (env fallbacks in parens):
  -id ID            peer id on signaling server            (P2PTUN_ID)
  -peer ID          remote peer id (connect)               (P2PTUN_PEER)
  -secret S         pairing secret, must match both sides  (P2PTUN_SECRET)
  -target H:P       exposed TCP endpoint (expose)          (P2PTUN_TARGET=127.0.0.1:22)
  -listen H:P       local listen addr (connect)            (P2PTUN_LISTEN=127.0.0.1:2222)
  -broker CSV       signaling brokers, comma-separated     (P2PTUN_BROKER=%s)
  -key K            PeerJS api key                         (P2PTUN_KEY=peerjs)
  -stun CSV         STUN servers, comma-separated          (P2PTUN_STUN)
  -turn CSV         optional TURN servers (user:pass@url)  (P2PTUN_TURN)
  -force-relay      force TURN relay mode for strict NATs  (P2PTUN_FORCE_RELAY)
  -ice-ports MIN-MAX UDP port range for ICE candidates     (P2PTUN_ICE_PORTS)
  -nat1to1 IP       advertise external 1:1 NAT IP          (P2PTUN_NAT1TO1)
  -proxy URL        HTTP/HTTPS proxy for signaling WS      (P2PTUN_PROXY)
  -keep-proxy       keep system HTTP_PROXY for signaling   (P2PTUN_KEEP_PROXY)
  -keepalive DUR    DataChannel ping interval (default 10s)(P2PTUN_KEEPALIVE)
  -max-peers N      max concurrent peers on expose (16)    (P2PTUN_MAX_PEERS)
  -on-demand        connect only when local TCP connects   (P2PTUN_ON_DEMAND)
  -idle-timeout DUR close idle on-demand tunnel (10m)      (P2PTUN_IDLE_TIMEOUT)
  -daemon, -d       run in background daemon mode          (P2PTUN_DAEMON)
  -pidfile PATH     write process PID to file              (P2PTUN_PIDFILE)
  -logfile PATH     rotating log output path               (P2PTUN_LOGFILE)
  -status-addr H:P  local HTTP status/health endpoint      (P2PTUN_STATUS_ADDR)
`, version, defaultBroker)
}

type config struct {
	mode        string
	id          string
	idExplicit  bool
	peer        string
	secret      string
	target      string
	listen      string
	broker      string
	brokers     []string
	key         string
	stun        []string
	turn        []string
	forceRelay  bool
	icePorts    string
	nat1to1     []string
	proxy       string
	keepProxy   bool
	keepalive   time.Duration
	maxPeers    int
	onDemand    bool
	idleTimeout time.Duration
	daemon      bool
	pidFile     string
	logFile     string
	logMaxMB    int
	statusAddr  string
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d
		}
	}
	return def
}

func parseConfig(args []string, mode string) (config, error) {
	fs := flag.NewFlagSet(mode, flag.ContinueOnError)
	cfg := config{
		mode:        mode,
		id:          envOr("P2PTUN_ID", ""),
		peer:        envOr("P2PTUN_PEER", ""),
		secret:      envOr("P2PTUN_SECRET", ""),
		target:      envOr("P2PTUN_TARGET", "127.0.0.1:22"),
		listen:      envOr("P2PTUN_LISTEN", "127.0.0.1:2222"),
		broker:      envOr("P2PTUN_BROKER", defaultBroker),
		key:         envOr("P2PTUN_KEY", defaultKey),
		stun:        splitCSV(envOr("P2PTUN_STUN", defaultSTUN)),
		turn:        splitCSV(envOr("P2PTUN_TURN", "")),
		forceRelay:  envBool("P2PTUN_FORCE_RELAY"),
		icePorts:    envOr("P2PTUN_ICE_PORTS", ""),
		nat1to1:     splitCSV(envOr("P2PTUN_NAT1TO1", "")),
		proxy:       envOr("P2PTUN_PROXY", ""),
		keepProxy:   envBool("P2PTUN_KEEP_PROXY"),
		keepalive:   envDuration("P2PTUN_KEEPALIVE", 10*time.Second),
		maxPeers:    envInt("P2PTUN_MAX_PEERS", 16),
		onDemand:    envBool("P2PTUN_ON_DEMAND"),
		idleTimeout: envDuration("P2PTUN_IDLE_TIMEOUT", 10*time.Minute),
		daemon:      envBool("P2PTUN_DAEMON"),
		pidFile:     envOr("P2PTUN_PIDFILE", ""),
		logFile:     envOr("P2PTUN_LOGFILE", ""),
		logMaxMB:    envInt("P2PTUN_LOG_MAX_MB", 10),
		statusAddr:  envOr("P2PTUN_STATUS_ADDR", ""),
	}
	fs.StringVar(&cfg.id, "id", cfg.id, "peer id")
	fs.StringVar(&cfg.peer, "peer", cfg.peer, "remote peer id (connect)")
	fs.StringVar(&cfg.secret, "secret", cfg.secret, "pairing secret")
	fs.StringVar(&cfg.target, "target", cfg.target, "target tcp endpoint (expose)")
	fs.StringVar(&cfg.listen, "listen", cfg.listen, "local listen addr (connect)")
	fs.StringVar(&cfg.broker, "broker", cfg.broker, "signaling broker URL(s), comma separated")
	fs.StringVar(&cfg.key, "key", cfg.key, "PeerJS api key")
	stun := ""
	fs.StringVar(&stun, "stun", "", "STUN servers, comma separated")
	turn := ""
	fs.StringVar(&turn, "turn", "", "TURN servers, comma separated")
	nat1to1 := ""
	fs.StringVar(&nat1to1, "nat1to1", "", "1:1 NAT external IPs, comma separated")
	fs.BoolVar(&cfg.forceRelay, "force-relay", cfg.forceRelay, "force TURN relay transport")
	fs.StringVar(&cfg.icePorts, "ice-ports", cfg.icePorts, "UDP port range min-max for ICE")
	fs.StringVar(&cfg.proxy, "proxy", cfg.proxy, "HTTP/HTTPS proxy URL for signaling")
	fs.BoolVar(&cfg.keepProxy, "keep-proxy", cfg.keepProxy, "keep ambient proxy for signaling")
	fs.DurationVar(&cfg.keepalive, "keepalive", cfg.keepalive, "DataChannel keepalive interval")
	fs.IntVar(&cfg.maxPeers, "max-peers", cfg.maxPeers, "maximum concurrent peers on expose")
	fs.BoolVar(&cfg.onDemand, "on-demand", cfg.onDemand, "connect on-demand when TCP client connects")
	fs.DurationVar(&cfg.idleTimeout, "idle-timeout", cfg.idleTimeout, "idle timeout for on-demand mode")
	fs.BoolVar(&cfg.daemon, "daemon", cfg.daemon, "run in background daemon mode")
	fs.BoolVar(&cfg.daemon, "d", cfg.daemon, "run in background daemon mode (shorthand)")
	fs.StringVar(&cfg.pidFile, "pidfile", cfg.pidFile, "PID file path")
	fs.StringVar(&cfg.logFile, "logfile", cfg.logFile, "rotating log file path")
	fs.IntVar(&cfg.logMaxMB, "log-max-mb", cfg.logMaxMB, "max log file size in MB before rotation")
	fs.StringVar(&cfg.statusAddr, "status-addr", cfg.statusAddr, "local HTTP status server address")

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}

	if stun != "" {
		cfg.stun = splitCSV(stun)
	}
	if turn != "" {
		cfg.turn = splitCSV(turn)
	}
	if nat1to1 != "" {
		cfg.nat1to1 = splitCSV(nat1to1)
	}
	cfg.brokers = splitCSV(cfg.broker)
	if len(cfg.brokers) == 0 {
		cfg.brokers = splitCSV(defaultBroker)
	}
	cfg.broker = cfg.brokers[0]

	if cfg.id != "" {
		cfg.idExplicit = true
	} else {
		cfg.id = map[string]string{"expose": "wslssh", "connect": "phone"}[mode] + "-" + randHex(4)
	}
	if mode == "connect" && cfg.peer == "" {
		return cfg, fmt.Errorf("connect: -peer REMOTE_ID is required")
	}
	if cfg.secret == "" {
		return cfg, fmt.Errorf("%s: -secret is required", mode)
	}
	if cfg.maxPeers <= 0 {
		cfg.maxPeers = 16
	}

	configuredProxy = cfg.proxy
	keepAmbientProxy = cfg.keepProxy
	return cfg, nil
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseICEServerEntry 支持解析带账号密码的 TURN URL:
//   - turn:user:pass@host:3478?transport=udp
//   - turns:user:pass@host:5349?transport=tcp
//   - user:pass@turn:host:3478
func parseICEServerEntry(raw string) webrtc.ICEServer {
	s := strings.TrimSpace(raw)
	for _, prefix := range []string{"turn:", "turns:"} {
		if strings.HasPrefix(strings.ToLower(s), prefix) {
			rest := s[len(prefix):]
			rest = strings.TrimPrefix(rest, "//")
			if at := strings.LastIndex(rest, "@"); at > 0 {
				userinfo := rest[:at]
				hostpart := rest[at+1:]
				user, pass := userinfo, ""
				if colon := strings.Index(userinfo, ":"); colon >= 0 {
					user = userinfo[:colon]
					pass = userinfo[colon+1:]
				}
				u, _ := url.QueryUnescape(user)
				p, _ := url.QueryUnescape(pass)
				return webrtc.ICEServer{
					URLs:           []string{prefix + hostpart},
					Username:       u,
					Credential:     p,
					CredentialType: webrtc.ICECredentialTypePassword,
				}
			}
		}
	}
	if at := strings.Index(s, "@"); at > 0 {
		credPart := s[:at]
		urlPart := s[at+1:]
		if strings.HasPrefix(strings.ToLower(urlPart), "turn:") || strings.HasPrefix(strings.ToLower(urlPart), "turns:") {
			user, pass := credPart, ""
			if colon := strings.Index(credPart, ":"); colon >= 0 {
				user = credPart[:colon]
				pass = credPart[colon+1:]
			}
			return webrtc.ICEServer{
				URLs:           []string{urlPart},
				Username:       user,
				Credential:     pass,
				CredentialType: webrtc.ICECredentialTypePassword,
			}
		}
	}
	return webrtc.ICEServer{URLs: []string{s}}
}

func (c config) iceServers() []webrtc.ICEServer {
	var servers []webrtc.ICEServer
	urls := c.stun
	if len(urls) == 0 {
		urls = splitCSV(defaultSTUN)
	}
	if !c.forceRelay && len(urls) > 0 {
		servers = append(servers, webrtc.ICEServer{URLs: urls})
	}
	for _, t := range c.turn {
		servers = append(servers, parseICEServerEntry(t))
	}
	return servers
}

// newPeerConn 使用 SettingEngine 构建针对多网络环境优化的 PeerConnection
func (c config) newPeerConn() (*webrtc.PeerConnection, error) {
	se := webrtc.SettingEngine{}
	// 放宽短暂断连容忍度 (10s disconnected -> 25s failed, 2s ICE keepalive),
	// 避免手机基站切换或瞬间丢包导致连接误断, 同时结合 DataChannel ping 快速识别真正断网
	se.SetICETimeouts(10*time.Second, 25*time.Second, 2*time.Second)

	if c.icePorts != "" {
		parts := strings.Split(c.icePorts, "-")
		if len(parts) == 2 {
			minP, err1 := strconv.ParseUint(strings.TrimSpace(parts[0]), 10, 16)
			maxP, err2 := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 16)
			if err1 == nil && err2 == nil && minP > 0 && maxP >= minP {
				_ = se.SetEphemeralUDPPortRange(uint16(minP), uint16(maxP))
			}
		}
	}
	if len(c.nat1to1) > 0 {
		se.SetNAT1To1IPs(c.nat1to1, webrtc.ICECandidateTypeHost)
	}

	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	wcfg := webrtc.Configuration{
		ICEServers: c.iceServers(),
	}
	if c.forceRelay && len(c.turn) > 0 {
		wcfg.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	}
	return api.NewPeerConnection(wcfg)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// backoffDelay 计算带随机抖动的指数退避等待时间, 兼顾快速恢复与省电防封
func backoffDelay(attempt int, base, max time.Duration) time.Duration {
	if attempt <= 0 {
		return base
	}
	shift := attempt
	if shift > 6 {
		shift = 6
	}
	d := base * time.Duration(1<<shift)
	if d > max {
		d = max
	}
	// +0~25% 随机抖动
	if jitterMax := int64(d / 4); jitterMax > 0 {
		if n, err := rand.Int(rand.Reader, big.NewInt(jitterMax)); err == nil {
			d += time.Duration(n.Int64())
		}
	}
	return d
}

// sleepOrStop 等待 d 或在 stop 关闭时立即唤醒返回 false
func sleepOrStop(d time.Duration, stop <-chan struct{}) bool {
	if d <= 0 {
		select {
		case <-stop:
			return false
		default:
			return true
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-stop:
		return false
	case <-t.C:
		return true
	}
}

// ---- shared SDP/ICE candidate state (buffers candidates until remote desc is set) ----

type sdpState struct {
	mu        sync.Mutex
	remoteSet bool
	pending   []webrtc.ICECandidateInit
}

func (s *sdpState) onCandidate(pc *webrtc.PeerConnection, init webrtc.ICECandidateInit) {
	if init.Candidate == "" {
		return
	}
	s.mu.Lock()
	if !s.remoteSet {
		s.pending = append(s.pending, init)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	if err := pc.AddICECandidate(init); err != nil {
		log.Printf("addIceCandidate: %v", err)
	}
}

func (s *sdpState) onRemoteSet(pc *webrtc.PeerConnection) {
	s.mu.Lock()
	toAdd := s.pending
	s.pending = nil
	s.remoteSet = true
	s.mu.Unlock()
	for _, c := range toAdd {
		if c.Candidate == "" {
			continue
		}
		if err := pc.AddICECandidate(c); err != nil {
			log.Printf("addIceCandidate(queued): %v", err)
		}
	}
}

func (s *sdpState) remoteLocked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.remoteSet
}

func candToPJ(c webrtc.ICECandidateInit) pjIce {
	return pjIce{
		Candidate:        c.Candidate,
		SDPMid:           str(c.SDPMid),
		SDPMLineIndex:    u16(c.SDPMLineIndex),
		UsernameFragment: str(c.UsernameFragment),
	}
}

func pjToCand(c pjIce) webrtc.ICECandidateInit {
	init := webrtc.ICECandidateInit{Candidate: c.Candidate}
	if c.SDPMid != "" {
		mid := c.SDPMid
		init.SDPMid = &mid
	}
	ml := c.SDPMLineIndex
	init.SDPMLineIndex = &ml
	if c.UsernameFragment != "" {
		ufrag := c.UsernameFragment
		init.UsernameFragment = &ufrag
	}
	return init
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
func u16(p *uint16) uint16 {
	if p == nil {
		return 0
	}
	return *p
}

// ============================================================================
// expose (runs next to the TCP service, supports persistent signaling +
//         concurrent multi-peer sessions + zero-stall instant reconnection)
// ============================================================================

func runExpose(args []string) {
	cfg, err := parseConfig(args, "expose")
	if err != nil {
		log.Fatalf("%v", err)
	}
	_, cleanup, err := maybeDaemonize(&cfg)
	if err != nil {
		log.Fatalf("expose daemon init: %v", err)
	}
	defer cleanup()

	stop := make(chan struct{})
	var stopOnce sync.Once
	closeStop := func() { stopOnce.Do(func() { close(stop) }) }

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	waitFirstDNSProbe(400 * time.Millisecond)
	mgr := newExposeManager(cfg, stop)
	globalStats.mode = "expose"
	globalStats.peerID = cfg.id
	globalStats.targetOrLis = cfg.target
	globalStats.setTriggerReconnect(mgr.forceSignalingReconnect)

	if cfg.statusAddr != "" {
		if _, err := startStatusServer(cfg.statusAddr, stop, closeStop); err != nil {
			log.Printf("expose: status server warning: %v", err)
		}
	}

	go func() {
		for sig := range sigCh {
			if sig == syscall.SIGHUP {
				log.Printf("expose: received SIGHUP, refreshing signaling connection")
				mgr.forceSignalingReconnect()
				continue
			}
			log.Printf("expose: received %v, shutting down gracefully...", sig)
			closeStop()
			return
		}
	}()

	go watchNetworkChanges(stop, 5*time.Second, func(oldFP, newFP string) {
		log.Printf("expose: local network changed (%s -> %s), refreshing signaling", oldFP, newFP)
		mgr.forceSignalingReconnect()
	})

	mgr.run()
}

// exposeSession 代表来自某个远端客户端的一条独立 WebRTC 隧道会话
type exposeSession struct {
	key      string
	peer     string
	connID   string
	cfg      config
	pc       *webrtc.PeerConnection
	sdp      *sdpState
	reg      *streamReg
	ctx      context.Context
	cancel   context.CancelFunc
	trMu     sync.Mutex
	tr       *transport
	dcOpened atomic.Bool
	closed   atomic.Bool
	done     chan struct{}
	once     sync.Once
	onDone   func(*exposeSession)
}

func (es *exposeSession) close() {
	es.once.Do(func() {
		es.closed.Store(true)
		if es.cancel != nil {
			es.cancel() // 取消该会话内所有挂起的拨号
		}
		close(es.done)
		es.trMu.Lock()
		tr := es.tr
		es.trMu.Unlock()
		if tr != nil {
			tr.closeNoNotify() // 防止 sync.Once 重入死锁 (close() 是 transport 的 onClose)
		}
		es.reg.closeAll()
		if es.pc != nil {
			_ = es.pc.Close()
		}
		if es.onDone != nil {
			es.onDone(es)
		}
	})
}

type exposeManager struct {
	cfg      config
	stop     <-chan struct{}
	mu       sync.Mutex
	sc       *signalingClient
	sessions map[string]*exposeSession
	reconnCh chan struct{}
}

func newExposeManager(cfg config, stop <-chan struct{}) *exposeManager {
	return &exposeManager{
		cfg:      cfg,
		stop:     stop,
		sessions: make(map[string]*exposeSession),
		reconnCh: make(chan struct{}, 1),
	}
}

func (m *exposeManager) forceSignalingReconnect() {
	select {
	case m.reconnCh <- struct{}{}:
	default:
	}
	m.mu.Lock()
	sc := m.sc
	m.mu.Unlock()
	if sc != nil {
		sc.shutdown()
	}
}

func (m *exposeManager) getSc() *signalingClient {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sc
}

func (m *exposeManager) closeAllSessions(notifyLeave bool) {
	m.mu.Lock()
	sc := m.sc
	list := make([]*exposeSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.sessions = make(map[string]*exposeSession)
	m.mu.Unlock()
	globalStats.activePeers.Store(0)
	for _, s := range list {
		if notifyLeave && sc != nil {
			sc.sendLeave(s.peer, s.connID)
		}
		s.close()
	}
}

// removeSession 只删除"自己"这个会话: 迟到的旧会话回调不会误删同 key 的新会话。
func (m *exposeManager) removeSession(sess *exposeSession) {
	m.mu.Lock()
	if cur, ok := m.sessions[sess.key]; ok && cur == sess {
		delete(m.sessions, sess.key)
	}
	n := len(m.sessions)
	m.mu.Unlock()
	globalStats.activePeers.Store(int64(n))
}

func (m *exposeManager) run() {
	log.Printf("expose: id=%s target=%s brokers=%s max-peers=%d secret=***",
		m.cfg.id, m.cfg.target, strings.Join(m.cfg.brokers, ","), m.cfg.maxPeers)
	defer m.closeAllSessions(true)

	attempt := 0
	for {
		select {
		case <-m.stop:
			return
		default:
		}

		started := time.Now()
		err := m.runSignalingLoop()
		globalStats.signalingUp.Store(false)
		if err != nil {
			globalStats.setError(err)
		}

		select {
		case <-m.stop:
			return
		default:
		}

		// 若稳定运行了超过 30 秒后断开, 重置退避计数立即重连
		if time.Since(started) > 30*time.Second {
			attempt = 0
		}

		wait := backoffDelay(attempt, 1*time.Second, 15*time.Second)
		if errors.Is(err, errIDTaken) {
			// PeerJS 云服务器在异常断网后可能残留旧 WebSocket 约 10~30s, 退避等待服务端清理
			wait = backoffDelay(attempt+2, 2*time.Second, 20*time.Second)
		}
		attempt++
		globalStats.reconnects.Add(1)
		log.Printf("expose: signaling disconnected: %v (reconnect in %v)", err, wait.Round(100*time.Millisecond))

		select {
		case <-m.stop:
			return
		case <-m.reconnCh:
			attempt = 0
		case <-time.After(wait):
		}
	}
}

func (m *exposeManager) runSignalingLoop() error {
	token := randHex(12)
	sigLost := make(chan error, 1)

	sc, err := dialSignalingAny(
		m.cfg.brokers,
		m.cfg.key,
		m.cfg.id,
		token,
		func(typ, src string, sig pjSignal) {
			m.handleSignal(typ, src, sig)
		},
		func(err error) {
			select {
			case sigLost <- err:
			default:
			}
		},
	)
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.sc = sc
	m.mu.Unlock()
	defer func() {
		sc.shutdown()
		m.mu.Lock()
		if m.sc == sc {
			m.sc = nil
		}
		m.mu.Unlock()
	}()

	globalStats.signalingUp.Store(true)
	globalStats.currentBroker.Store(sc.broker)
	log.Printf("expose: signaling ready id=%s broker=%s", m.cfg.id, sc.broker)
	// 配对密钥绝不能进日志: 日志默认落在 ~/.p2ptun/, 历史版本还是 0644 可读.
	// 这里只打印可直接复制的命令骨架, 密钥由使用者自行填入.
	log.Printf("expose: connect side command: p2ptun connect -peer %s -secret <SECRET> -listen 127.0.0.1:2222",
		m.cfg.id)

	select {
	case <-m.stop:
		m.closeAllSessions(true)
		return nil
	case <-m.reconnCh:
		return fmt.Errorf("manual or network-change signaling refresh")
	case err := <-sigLost:
		return err
	}
}

func sessionKey(peer, connID string) string {
	if connID == "" {
		return peer + "/default"
	}
	return peer + "/" + connID
}

func (m *exposeManager) findSession(peer, connID string) *exposeSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	if connID != "" {
		if s, ok := m.sessions[sessionKey(peer, connID)]; ok {
			return s
		}
	}
	// 兼容未带 connID 的候选帧: 若该 peer 只有一个活跃会话则匹配它
	var match *exposeSession
	for _, s := range m.sessions {
		if s.peer == peer {
			if match != nil {
				return nil
			}
			match = s
		}
	}
	return match
}

func (m *exposeManager) handleSignal(typ, src string, sig pjSignal) {
	switch typ {
	case pjOffer:
		if sig.Metadata != m.cfg.secret {
			log.Printf("expose: REJECT offer from %s: bad secret", src)
			if sc := m.getSc(); sc != nil {
				sc.sendLeave(src, sig.ConnectionID)
			}
			return
		}
		m.acceptOffer(src, sig)

	case pjCandidate:
		if sess := m.findSession(src, sig.ConnectionID); sess != nil {
			sess.sdp.onCandidate(sess.pc, pjToCand(sig.Candidate))
		}

	case pjLeave:
		if sess := m.findSession(src, sig.ConnectionID); sess != nil {
			log.Printf("expose: peer %s (conn=%s) sent LEAVE, closing session", src, sig.ConnectionID)
			sess.close()
		}
	}
}

// acceptOffer 为每次新 OFFER 立即创建全新独立的 PeerConnection:
//   - 彻底解决原版 "busy pc=.., restarting session" 丢弃新 OFFER 并重启整个信令导致的 18s+ 卡死;
//   - 支持同一客户端无缝秒级重连, 以及多个不同客户端同时并发连接!
func (m *exposeManager) acceptOffer(src string, sig pjSignal) {
	key := sessionKey(src, sig.ConnectionID)

	// 清理同一 (src, connID) 的旧会话, 或同一 src 处于非活跃/正在连接状态的旧会话
	var stale []*exposeSession
	m.mu.Lock()
	for k, existing := range m.sessions {
		if k == key || (existing.peer == src && (!existing.dcOpened.Load() ||
			existing.pc.ConnectionState() != webrtc.PeerConnectionStateConnected)) {
			stale = append(stale, existing)
			delete(m.sessions, k)
		}
	}
	// 若总会话数超出 maxPeers, 淘汰最早的一个非 Connected 会话
	if len(m.sessions) >= m.cfg.maxPeers {
		for k, existing := range m.sessions {
			if existing.peer == src || existing.pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
				stale = append(stale, existing)
				delete(m.sessions, k)
				break
			}
		}
	}
	if len(m.sessions) >= m.cfg.maxPeers {
		m.mu.Unlock()
		log.Printf("expose: max peers (%d) reached, rejecting OFFER from %s", m.cfg.maxPeers, src)
		return
	}
	m.mu.Unlock()

	for _, old := range stale {
		log.Printf("expose: replacing stale session %s for fresh OFFER", old.key)
		old.close()
	}

	pc, err := m.cfg.newPeerConn()
	if err != nil {
		log.Printf("expose: newPeerConn for %s: %v", src, err)
		return
	}

	sessCtx, sessCancel := context.WithCancel(context.Background())
	sess := &exposeSession{
		key:    key,
		peer:   src,
		connID: sig.ConnectionID,
		cfg:    m.cfg,
		pc:     pc,
		sdp:    &sdpState{},
		reg:    newStreamReg(),
		ctx:    sessCtx,
		cancel: sessCancel,
		done:   make(chan struct{}),
		onDone: m.removeSession,
	}

	m.mu.Lock()
	m.sessions[key] = sess
	peerCount := len(m.sessions)
	m.mu.Unlock()
	globalStats.activePeers.Store(int64(peerCount))

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnOpen(func() {
			log.Printf("expose: [%s] datachannel open", key)
			sess.dcOpened.Store(true)
			// handler 在 OnMessage 注册前拿到 transport 自身, 首帧不会因为
			// sess.tr 尚未赋值而被丢弃 (OnOpen 与 OnMessage 在不同协程).
			t := newTransport(dc, func(t *transport) frameHandler {
				return func(typ uint8, id uint32, payload []byte) {
					if sess.closed.Load() {
						return
					}
					handleFrameExpose(m.cfg, sess.reg, t, sess.ctx, typ, id, payload)
				}
			}, sess.close)
			t.startKeepalive(m.cfg.keepalive)
			sess.trMu.Lock()
			sess.tr = t
			sess.trMu.Unlock()
		})
	})

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil || sess.closed.Load() {
			return
		}
		sc := m.getSc()
		if sc == nil {
			return
		}
		_ = sc.sendSignal(pjCandidate, src, pjSignal{
			Type:         "data",
			ConnectionID: sig.ConnectionID,
			Candidate:    candToPJ(c.ToJSON()),
		})
	})

	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		log.Printf("expose: [%s] pc state=%s", key, st)
		switch st {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			sess.close()
		case webrtc.PeerConnectionStateDisconnected:
			// 若处于 Disconnected 超过 12s 未恢复, 主动回收该会话
			go func() {
				if !sleepOrStop(12*time.Second, sess.done) {
					return
				}
				if pc.ConnectionState() == webrtc.PeerConnectionStateDisconnected {
					log.Printf("expose: [%s] ICE disconnected >12s, closing session", key)
					sess.close()
				}
			}()
		}
	})

	// 半通自愈: 25s 内 DataChannel 未就绪则回收该会话 (不影响其他在线客户端与信令常驻!)
	go func() {
		if !sleepOrStop(25*time.Second, sess.done) {
			return
		}
		if !sess.dcOpened.Load() {
			log.Printf("expose: [%s] datachannel not opened within 25s (pc=%s), cleaning up session",
				key, pc.ConnectionState())
			sess.close()
		}
	}()

	log.Printf("expose: OFFER from %s conn=%s (active peers=%d)", src, sig.ConnectionID, peerCount)
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sig.SDP.SDP}); err != nil {
		log.Printf("expose: [%s] setRemoteDescription: %v", key, err)
		sess.close()
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		log.Printf("expose: [%s] createAnswer: %v", key, err)
		sess.close()
		return
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		log.Printf("expose: [%s] setLocalDescription: %v", key, err)
		sess.close()
		return
	}
	sc := m.getSc()
	if sc == nil {
		sess.close()
		return
	}
	if err := sc.sendSignal(pjAnswer, src, pjSignal{
		Type:         "data",
		ConnectionID: sig.ConnectionID,
		SDP:          pjSDP{Type: "answer", SDP: answer.SDP},
	}); err != nil {
		log.Printf("expose: [%s] send ANSWER: %v", key, err)
		sess.close()
		return
	}
	sess.sdp.onRemoteSet(pc)
}

func handleFrameExpose(cfg config, reg *streamReg, tr *transport, sessCtx context.Context, typ uint8, id uint32, payload []byte) {
	if sessCtx == nil {
		sessCtx = context.Background()
	}
	switch typ {
	case frameSyn:
		// 限制单会话并发流数, 避免对端洪泛 frameSyn 时每个都挂一个 5s 拨号协程和 FD.
		if reg.count() >= maxStreamsPerSession {
			log.Printf("expose: too many streams (%d), rejecting SYN id=%d", reg.count(), id)
			_ = tr.send(frameRst, id, nil)
			return
		}
		st := newStream(id, nil)
		reg.add(st)
		go func() {
			dialCtx, cancel := context.WithCancel(sessCtx)
			defer cancel()
			watched := make(chan struct{})
			go func() {
				select {
				case <-st.stopped:
					cancel() // RST/会话关闭时立即取消挂起拨号
				case <-watched:
				}
			}()
			conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(dialCtx, "tcp", cfg.target)
			close(watched)
			if err != nil {
				log.Printf("expose: dial %s: %v", cfg.target, err)
				_ = tr.send(frameRst, id, nil)
				st.stop()
				reg.remove(id)
				return
			}
			if !st.attachConn(conn) {
				reg.remove(id)
				return
			}
			cleanup := func() { reg.remove(id) }
			go connToDC(conn, id, st, func(tt uint8, p []byte) error {
				return tr.send(tt, id, p)
			}, cleanup)
			go dcToConn(conn, st, func() {
				_ = tr.send(frameRst, id, nil)
			}, cleanup)
		}()
	case frameData:
		st := reg.get(id)
		if st == nil {
			return
		}
		if !st.enqueue(payload) {
			_ = tr.send(frameRst, id, nil)
			reg.remove(id)
		}
	case frameFin:
		if st := reg.get(id); st != nil {
			st.halfClose()
		}
	case frameRst:
		if st := reg.remove(id); st != nil {
			st.stop()
		}
	}
}

// ============================================================================
// connect (runs on the remote client side, e.g. Termux / laptop)
// ============================================================================

type pendingConn struct {
	conn     net.Conn
	accepted time.Time
}

func runConnect(args []string) {
	cfg, err := parseConfig(args, "connect")
	if err != nil {
		log.Fatalf("%v", err)
	}
	_, cleanup, err := maybeDaemonize(&cfg)
	if err != nil {
		log.Fatalf("connect daemon init: %v", err)
	}
	defer cleanup()

	log.Printf("connect: id=%s peer=%s listen=%s brokers=%s on-demand=%v secret=***",
		cfg.id, cfg.peer, cfg.listen, strings.Join(cfg.brokers, ","), cfg.onDemand)

	lis, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		log.Fatalf("connect: listen %s: %v", cfg.listen, err)
	}
	defer lis.Close()
	log.Printf("connect: listening on %s (ssh -p %s user@127.0.0.1)", cfg.listen, portOf(cfg.listen))

	stop := make(chan struct{})
	var stopOnce sync.Once
	closeStop := func() { stopOnce.Do(func() { close(stop) }) }

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	waitFirstDNSProbe(400 * time.Millisecond)
	globalStats.mode = "connect"
	globalStats.peerID = cfg.id
	globalStats.remotePeer = cfg.peer
	globalStats.targetOrLis = cfg.listen

	connCh := make(chan pendingConn, 128)
	wakeCh := make(chan struct{}, 1)
	reconnCh := make(chan struct{}, 1)

	var activeSessMu sync.Mutex
	var activeSess *tunnelSess
	forceReconnect := func() {
		select {
		case reconnCh <- struct{}{}:
		default:
		}
		activeSessMu.Lock()
		s := activeSess
		activeSessMu.Unlock()
		if s != nil {
			s.cancelFn()
		}
	}
	globalStats.setTriggerReconnect(forceReconnect)

	// shutdown 是完整优雅退出: 关 stop、关监听、取消当前会话.
	// 信号与 status server 的 /quit 共用, 避免 /quit 只关 stop 而主循环
	// 仍卡在 sess.run() 里退不出来 (Windows 上 stop 命令依赖 /quit).
	shutdown := func() {
		closeStop()
		_ = lis.Close()
		activeSessMu.Lock()
		s := activeSess
		activeSessMu.Unlock()
		if s != nil {
			s.cancelFn()
		}
	}

	if cfg.statusAddr != "" {
		if _, err := startStatusServer(cfg.statusAddr, stop, shutdown); err != nil {
			log.Printf("connect: status server warning: %v", err)
		}
	}

	go func() {
		for sig := range sigCh {
			if sig == syscall.SIGHUP {
				log.Printf("connect: received SIGHUP, triggering fast reconnect")
				forceReconnect()
				continue
			}
			log.Printf("connect: received %v, shutting down gracefully...", sig)
			shutdown()
			return
		}
	}()

	go watchNetworkChanges(stop, 5*time.Second, func(oldFP, newFP string) {
		log.Printf("connect: local network changed (%s -> %s), reconnecting immediately", oldFP, newFP)
		forceReconnect()
	})

	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				select {
				case <-stop:
					return
				default:
					log.Printf("connect: accept: %v", err)
					return
				}
			}
			tuneTCPConn(c)
			// 先入队再唤醒: 主循环醒来时 len(connCh) 必须已经反映这次连接.
			select {
			case connCh <- pendingConn{conn: c, accepted: time.Now()}:
				select {
				case wakeCh <- struct{}{}:
				default:
				}
			default:
				_ = c.Close()
			}
		}
	}()

	attempt := 0
	for {
		select {
		case <-stop:
			return
		default:
		}

		// 按需连接模式 (-on-demand): 当没有本地 TCP 连接等待时休眠省电, 有连接到达时立即唤醒打洞
		if cfg.onDemand && len(connCh) == 0 {
			// 隧道在线期间到达的连接会留下一个没人消费的 wakeCh 信号,
			// 先排空它, 避免休眠后立刻假唤醒; 排空后二次确认队列仍为空再阻塞.
			select {
			case <-wakeCh:
			default:
			}
			if len(connCh) == 0 {
				log.Printf("connect: on-demand mode idle, waiting for local TCP connection on %s...", cfg.listen)
				select {
				case <-stop:
					return
				case <-wakeCh:
					log.Printf("connect: local TCP connection arrived, waking up tunnel...")
				}
			}
		}

		sess := newTunnelSess(cfg, connCh, cfg.peer, stop)
		activeSessMu.Lock()
		activeSess = sess
		activeSessMu.Unlock()

		err := sess.run()
		wasReady := sess.wasReady.Load()
		idleClosed := sess.idleClosed.Load()
		sess.teardown()

		activeSessMu.Lock()
		if activeSess == sess {
			activeSess = nil
		}
		activeSessMu.Unlock()

		select {
		case <-stop:
			return
		default:
		}

		if idleClosed && cfg.onDemand {
			attempt = 0
			continue
		}

		if err != nil {
			globalStats.setError(err)
		}

		// 若由于自动生成的 ID 撞名导致 ID-TAKEN, 立即换一个新 ID 重试, 无需等待
		if errors.Is(err, errIDTaken) && !cfg.idExplicit {
			cfg.id = "phone-" + randHex(4)
			globalStats.peerID = cfg.id
			log.Printf("connect: peer id taken, rotated client id to %s", cfg.id)
			attempt = 0
			continue
		}

		// 若刚刚处于已连通状态后断开 (例如手机切网或短暂闪断), 首次重连仅等 400ms 实现极速恢复!
		var wait time.Duration
		if wasReady {
			attempt = 0
			wait = 400 * time.Millisecond
		} else {
			wait = backoffDelay(attempt, 1*time.Second, 15*time.Second)
			if errors.Is(err, errIDTaken) {
				wait = backoffDelay(attempt+2, 2*time.Second, 20*time.Second)
			}
			attempt++
		}
		globalStats.reconnects.Add(1)
		log.Printf("connect: session ended: %v (retry in %v)", err, wait.Round(100*time.Millisecond))

		select {
		case <-stop:
			return
		case <-reconnCh:
			attempt = 0
		case <-time.After(wait):
		}
	}
}

func portOf(addr string) string {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return p
}

type tunnelSess struct {
	cfg        config
	connCh     chan pendingConn
	peer       string
	connID     string
	token      string
	pc         *webrtc.PeerConnection
	scMu       sync.Mutex
	sc         *signalingClient
	trMu       sync.Mutex
	tr         *transport
	trReady    chan struct{}
	readyOnce  sync.Once
	wasReady   atomic.Bool
	idleClosed atomic.Bool
	lastActive atomic.Int64
	reg        *streamReg
	cancel     chan struct{}
	stop       <-chan struct{}
	once       sync.Once
}

func newTunnelSess(cfg config, connCh chan pendingConn, peer string, stop <-chan struct{}) *tunnelSess {
	s := &tunnelSess{
		cfg:     cfg,
		connCh:  connCh,
		peer:    peer,
		connID:  "p2p-" + randHex(6),
		token:   randHex(12),
		trReady: make(chan struct{}),
		reg:     newStreamReg(),
		cancel:  make(chan struct{}),
		stop:    stop,
	}
	s.lastActive.Store(time.Now().UnixNano())
	return s
}

func (s *tunnelSess) setSc(sc *signalingClient) {
	s.scMu.Lock()
	s.sc = sc
	s.scMu.Unlock()
}

func (s *tunnelSess) getSc() *signalingClient {
	s.scMu.Lock()
	defer s.scMu.Unlock()
	return s.sc
}

func (s *tunnelSess) cancelFn() { s.once.Do(func() { close(s.cancel) }) }

func (s *tunnelSess) run() error {
	pc, err := s.cfg.newPeerConn()
	if err != nil {
		return err
	}
	s.pc = pc
	sdp := &sdpState{}

	dc, err := pc.CreateDataChannel("p2ptun", &webrtc.DataChannelInit{Ordered: boolPtr(true)})
	if err != nil {
		return err
	}
	dc.OnOpen(func() {
		log.Printf("connect: datachannel open (conn=%s)", s.connID)
		s.wasReady.Store(true)
		s.lastActive.Store(time.Now().UnixNano())
		globalStats.activePeers.Store(1)
		t := newTransport(dc, func(t *transport) frameHandler {
			return func(typ uint8, id uint32, payload []byte) {
				s.lastActive.Store(time.Now().UnixNano())
				handleFrameConnect(s.reg, t, typ, id, payload)
			}
		}, s.cancelFn)
		t.startKeepalive(s.cfg.keepalive)
		s.trMu.Lock()
		s.tr = t
		s.trMu.Unlock()
		s.readyOnce.Do(func() { close(s.trReady) })
	})
	dc.OnClose(func() { s.cancelFn() })

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		sc := s.getSc()
		if sc == nil {
			return
		}
		_ = sc.sendSignal(pjCandidate, s.peer, pjSignal{
			Type:         "data",
			ConnectionID: s.connID,
			Candidate:    candToPJ(c.ToJSON()),
		})
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		log.Printf("connect: pc state=%s (conn=%s)", st, s.connID)
		switch st {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			s.cancelFn()
		case webrtc.PeerConnectionStateDisconnected:
			go func() {
				if !sleepOrStop(10*time.Second, s.cancel) {
					return
				}
				if pc.ConnectionState() == webrtc.PeerConnectionStateDisconnected {
					log.Printf("connect: ICE disconnected >10s, restarting session")
					s.cancelFn()
				}
			}()
		}
	})
	// ICE 握手超时自愈: 15s 内 DataChannel 未就绪则自动重发新会话
	go func() {
		if !sleepOrStop(15*time.Second, s.cancel) {
			return
		}
		if !s.wasReady.Load() {
			log.Printf("connect: tunnel not ready after 15s (pc=%s), restarting session", pc.ConnectionState())
			s.cancelFn()
		}
	}()

	// 按需模式下若超过 idleTimeout 无活跃流, 自动挂起连接省电
	if s.cfg.onDemand && s.cfg.idleTimeout > 0 {
		go func() {
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-s.cancel:
					return
				case <-ticker.C:
					if s.wasReady.Load() && s.reg.count() == 0 {
						idle := time.Since(time.Unix(0, s.lastActive.Load()))
						if idle >= s.cfg.idleTimeout {
							log.Printf("connect: on-demand tunnel idle for %v, hibernating", idle.Round(time.Second))
							s.idleClosed.Store(true)
							s.cancelFn()
							return
						}
					}
				}
			}
		}()
	}

	sc, err := dialSignalingAny(s.cfg.brokers, s.cfg.key, s.cfg.id, s.token, func(typ, src string, sig pjSignal) {
		s.onSignal(typ, src, sig, sdp)
	}, func(err error) {
		// 信令断开时: 如果 WebRTC DataChannel 已经建立直连, P2P 数据面无需因信令闪断而自杀!
		if s.wasReady.Load() {
			log.Printf("connect: signaling ws closed (%v), keeping active P2P datachannel alive", err)
			globalStats.signalingUp.Store(false)
			return
		}
		log.Printf("connect: signaling closed before P2P ready: %v", err)
		s.cancelFn()
	})
	if err != nil {
		return err
	}
	s.setSc(sc)
	globalStats.signalingUp.Store(true)
	globalStats.currentBroker.Store(sc.broker)
	defer func() {
		globalStats.signalingUp.Store(false)
		sc.shutdown()
	}()

	log.Printf("connect: signaling ready (%s), sending OFFER to %s (conn=%s)", sc.broker, s.peer, s.connID)
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return err
	}
	reliable := true
	if err := sc.sendSignal(pjOffer, s.peer, pjSignal{
		SDP:           pjSDP{Type: "offer", SDP: offer.SDP},
		Type:          "data",
		ConnectionID:  s.connID,
		Label:         "p2ptun",
		Metadata:      s.cfg.secret,
		Reliable:      &reliable,
		Serialization: "default",
	}); err != nil {
		return err
	}

	go s.acceptLoop()
	select {
	case <-s.cancel:
		return fmt.Errorf("session over")
	case <-s.stop:
		return nil
	}
}

func (s *tunnelSess) onSignal(typ, src string, sig pjSignal, sdp *sdpState) {
	if src != "" && src != s.peer {
		return
	}
	if sig.ConnectionID != "" && sig.ConnectionID != s.connID {
		return
	}
	switch typ {
	case pjAnswer:
		if sdp.remoteLocked() {
			return
		}
		log.Printf("connect: ANSWER from %s (conn=%s)", src, s.connID)
		if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sig.SDP.SDP}); err != nil {
			log.Printf("connect: setRemoteDescription: %v", err)
			s.cancelFn()
			return
		}
		sdp.onRemoteSet(s.pc)
	case pjCandidate:
		sdp.onCandidate(s.pc, pjToCand(sig.Candidate))
	case pjLeave:
		log.Printf("connect: peer %s sent LEAVE (conn=%s)", src, sig.ConnectionID)
		s.cancelFn()
	}
}

// acceptLoop 仅在 DataChannel 就绪 (<-s.trReady) 后才从队列取出本地 TCP 连接,
// 避免在重连握手期间误取并直接关闭用户的 SSH 连接请求!
func (s *tunnelSess) acceptLoop() {
	select {
	case <-s.trReady:
	case <-s.cancel:
		return
	case <-s.stop:
		return
	}

	for {
		select {
		case <-s.cancel:
			return
		case <-s.stop:
			return
		case item := <-s.connCh:
			if item.conn == nil {
				return
			}
			// 如果该连接在队列里等待太久 (>30s), 关闭丢弃
			if time.Since(item.accepted) > 30*time.Second {
				_ = item.conn.Close()
				continue
			}
			s.registerConn(item)
		}
	}
}

func (s *tunnelSess) requeue(item pendingConn) {
	select {
	case s.connCh <- item:
	default:
		_ = item.conn.Close()
	}
}

func (s *tunnelSess) registerConn(item pendingConn) {
	s.trMu.Lock()
	tr := s.tr
	s.trMu.Unlock()
	if tr == nil {
		// 隧道刚断, 连接放回队列供下一轮重连使用; 立即结束本会话,
		// 否则 acceptLoop 会从队列里取出同一连接并立即失败, 形成忙等自旋.
		s.requeue(item)
		s.cancelFn()
		return
	}
	if s.reg.count() >= maxStreamsPerSession {
		log.Printf("connect: too many streams (%d), closing local conn", s.reg.count())
		_ = item.conn.Close()
		return
	}

	c := item.conn
	id := s.reg.allocID()
	st := newStream(id, c)
	s.reg.add(st)

	// 关键修复: 必须先发送 frameSyn 再启动 connToDC 读取协程!
	// 原版先起 go connToDC 再发 frameSyn, 导致客户端首包 (frameData) 可能早于 frameSyn 到达对端被静默丢弃!
	if err := tr.send(frameSyn, id, nil); err != nil {
		log.Printf("connect: SYN failed: %v, re-queueing connection", err)
		s.reg.remove(id)
		s.requeue(item)
		s.cancelFn() // 结束本会话, 由下一轮新会话消费队列中的连接
		return
	}

	s.lastActive.Store(time.Now().UnixNano())
	cleanup := func() { s.reg.remove(id) }
	go connToDC(c, id, st, func(tt uint8, p []byte) error {
		s.lastActive.Store(time.Now().UnixNano())
		return tr.send(tt, id, p)
	}, cleanup)
	go dcToConn(c, st, func() {
		_ = tr.send(frameRst, id, nil)
	}, cleanup)
}

func (s *tunnelSess) teardown() {
	s.cancelFn()
	globalStats.activePeers.Store(0)
	if sc := s.getSc(); sc != nil {
		sc.sendLeave(s.peer, s.connID)
		sc.shutdown()
	}
	s.trMu.Lock()
	tr := s.tr
	s.trMu.Unlock()
	if tr != nil {
		tr.close()
	}
	s.reg.closeAll()
	if s.pc != nil {
		_ = s.pc.Close()
	}
}

func handleFrameConnect(reg *streamReg, tr *transport, typ uint8, id uint32, payload []byte) {
	switch typ {
	case frameData:
		st := reg.get(id)
		if st == nil {
			return
		}
		if !st.enqueue(payload) {
			_ = tr.send(frameRst, id, nil)
			reg.remove(id)
		}
	case frameFin:
		if st := reg.get(id); st != nil {
			st.halfClose()
		}
	case frameRst:
		if st := reg.remove(id); st != nil {
			st.stop()
		}
	}
}

func boolPtr(b bool) *bool { return &b }
