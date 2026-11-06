package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ---- Mock PeerJS Server for In-Process Integration Tests ----

type mockPeerJSServer struct {
	srv      *httptest.Server
	upgrader websocket.Upgrader

	mu      sync.Mutex
	clients map[string]*websocket.Conn
	writeMu map[string]*sync.Mutex
}

func newMockPeerJSServer() *mockPeerJSServer {
	m := &mockPeerJSServer{
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		clients: make(map[string]*websocket.Conn),
		writeMu: make(map[string]*sync.Mutex),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/peerjs", m.handleWS)
	m.srv = httptest.NewServer(mux)
	return m
}

func (m *mockPeerJSServer) wsURL() string {
	return "ws" + strings.TrimPrefix(m.srv.URL, "http")
}

func (m *mockPeerJSServer) close() {
	m.mu.Lock()
	for _, c := range m.clients {
		_ = c.Close()
	}
	m.clients = make(map[string]*websocket.Conn)
	m.mu.Unlock()
	m.srv.Close()
}

func (m *mockPeerJSServer) handleWS(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	ws, err := m.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	m.mu.Lock()
	if _, exists := m.clients[id]; exists {
		m.mu.Unlock()
		_ = ws.WriteJSON(pjMessage{
			Type:    pjIDTaken,
			Payload: json.RawMessage(`{"msg":"ID is taken"}`),
		})
		_ = ws.Close()
		return
	}
	wmu := &sync.Mutex{}
	m.clients[id] = ws
	m.writeMu[id] = wmu
	m.mu.Unlock()

	wmu.Lock()
	_ = ws.WriteJSON(pjMessage{Type: pjOpen})
	wmu.Unlock()

	defer func() {
		m.mu.Lock()
		if m.clients[id] == ws {
			delete(m.clients, id)
			delete(m.writeMu, id)
		}
		m.mu.Unlock()
		_ = ws.Close()
	}()

	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		var msg pjMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.Type == pjHeartbeat {
			wmu.Lock()
			_ = ws.WriteJSON(pjMessage{Type: pjHeartbeat})
			wmu.Unlock()
			continue
		}
		if msg.Dst != "" {
			msg.Src = id
			m.mu.Lock()
			dstWS := m.clients[msg.Dst]
			dstMu := m.writeMu[msg.Dst]
			m.mu.Unlock()
			if dstWS != nil && dstMu != nil {
				dstMu.Lock()
				_ = dstWS.WriteJSON(msg)
				dstMu.Unlock()
			}
		}
	}
}

// ---- Unit Tests: DNS Probing, Broker URL, TURN Parsing, Stream Lifecycle ----

func TestBrokerURLAndTURNConfig(t *testing.T) {
	u1 := buildPeerJSURL("wss://0.peerjs.com", "peerjs", "my-id", "tok1")
	if !strings.HasPrefix(u1, "wss://0.peerjs.com/peerjs?key=peerjs&id=my-id&token=tok1") {
		t.Fatalf("unexpected url: %s", u1)
	}
	u2 := buildPeerJSURL("https://example.com/custom/peerjs/", "k", "id2", "t2")
	if !strings.HasPrefix(u2, "wss://example.com/custom/peerjs?key=k&id=id2&token=t2") {
		t.Fatalf("unexpected url with custom path: %s", u2)
	}

	ice1 := parseICEServerEntry("turn:alice:secret123@turn.example.com:3478?transport=udp")
	if len(ice1.URLs) != 1 || ice1.URLs[0] != "turn:turn.example.com:3478?transport=udp" ||
		ice1.Username != "alice" || ice1.Credential != "secret123" {
		t.Fatalf("unexpected parsed TURN server: %+v", ice1)
	}

	ice2 := parseICEServerEntry("bob:pw@turns:turn.example.com:5349?transport=tcp")
	if len(ice2.URLs) != 1 || ice2.URLs[0] != "turns:turn.example.com:5349?transport=tcp" ||
		ice2.Username != "bob" || ice2.Credential != "pw" {
		t.Fatalf("unexpected parsed TURNS server: %+v", ice2)
	}
}

func TestDNSProbeAndTCPFallback(t *testing.T) {
	tcpLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	defer tcpLis.Close()

	go func() {
		for {
			c, err := tcpLis.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				var lenBuf [2]byte
				if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
					return
				}
				qLen := binary.BigEndian.Uint16(lenBuf[:])
				q := make([]byte, qLen)
				if _, err := io.ReadFull(conn, q); err != nil {
					return
				}
				resp := make([]byte, 2+len(q))
				binary.BigEndian.PutUint16(resp[:2], uint16(len(q)))
				copy(resp[2:], q)
				if len(q) >= 3 {
					resp[4] = 0x81
				}
				_, _ = conn.Write(resp)
			}(c)
		}
	}()

	deadAddr := "127.0.0.1:59999"
	goodAddr := tcpLis.Addr().String()
	setActiveDNS([]string{deadAddr, goodAddr})

	probeDNSServers(300 * time.Millisecond)
	active := getActiveDNS()
	if len(active) < 2 || active[0] != goodAddr {
		t.Fatalf("expected healthy TCP DNS %s to be ranked first, got %v", goodAddr, active)
	}
	if !preferTCPDNS.Load() {
		t.Fatalf("expected preferTCPDNS=true when only TCP DNS responded")
	}
	preferTCPDNS.Store(false)
}

func TestStreamHalfCloseAndStopNoLeak(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	st := newStream(1, c1)
	st.activePumps.Store(1)
	doneCh := make(chan struct{})
	go dcToConn(c1, st, nil, func() { close(doneCh) })

	payload := []byte("hello-half-close")
	if !st.enqueue(payload) {
		t.Fatalf("enqueue failed")
	}
	st.halfClose()

	buf := make([]byte, 64)
	n, err := c2.Read(buf)
	if err != nil || !bytes.Equal(buf[:n], payload) {
		t.Fatalf("unexpected read: n=%d err=%v", n, err)
	}

	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("dcToConn did not exit after halfClose drain")
	}

	c3, c4 := net.Pipe()
	defer c3.Close()
	defer c4.Close()
	st2 := newStream(2, c3)
	st2.activePumps.Store(1)
	doneCh2 := make(chan struct{})
	go dcToConn(c3, st2, nil, func() { close(doneCh2) })
	st2.stop()

	select {
	case <-doneCh2:
	case <-time.After(2 * time.Second):
		t.Fatalf("dcToConn leaked goroutine after st.stop()")
	}
}

func TestRotatingLogAndStatusServer(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")
	rw, err := newRotatingFileWriter(logPath, 1)
	if err != nil {
		t.Fatalf("newRotatingFileWriter: %v", err)
	}
	rw.maxSize = 256
	chunk := bytes.Repeat([]byte("A"), 150)
	_, _ = rw.Write(chunk)
	_, _ = rw.Write(chunk)
	_ = rw.Close()

	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("expected rotated backup file %s.1 to exist: %v", logPath, err)
	}

	stop := make(chan struct{})
	defer close(stop)
	globalStats.signalingUp.Store(true)
	lis, err := startStatusServer("127.0.0.1:0", stop, nil)
	if err != nil {
		t.Fatalf("startStatusServer: %v", err)
	}
	resp, err := http.Get("http://" + lis.Addr().String() + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status code: %d", resp.StatusCode)
	}
}

func TestControlPanelDisabledByDefaultAndFlags(t *testing.T) {
	// 默认不指定任何监听地址时, statusAddr 必须为空, 且 startStatusServer 返回 nil, nil (不开启)
	cfg, err := parseConfig([]string{"-secret", "sec", "-id", "id1"}, "expose")
	if err != nil {
		t.Fatalf("parseConfig error: %v", err)
	}
	if cfg.statusAddr != "" {
		t.Fatalf("expected statusAddr to be empty by default, got %q", cfg.statusAddr)
	}
	lis, err := startStatusServer(cfg.statusAddr, make(chan struct{}), nil)
	if err != nil {
		t.Fatalf("expected nil error on empty statusAddr, got: %v", err)
	}
	if lis != nil {
		t.Fatalf("expected nil listener on empty statusAddr, got: %v", lis)
	}

	// 指定 -panel-addr 时生效
	cfgPanel, err := parseConfig([]string{"-secret", "sec", "-id", "id1", "-panel-addr", "127.0.0.1:18225"}, "expose")
	if err != nil {
		t.Fatalf("parseConfig with -panel-addr error: %v", err)
	}
	if cfgPanel.statusAddr != "127.0.0.1:18225" {
		t.Fatalf("expected statusAddr=127.0.0.1:18225, got %q", cfgPanel.statusAddr)
	}

	// 指定 -control-addr 时生效
	cfgCtrl, err := parseConfig([]string{"-secret", "sec", "-id", "id1", "-control-addr", "127.0.0.1:18226"}, "expose")
	if err != nil {
		t.Fatalf("parseConfig with -control-addr error: %v", err)
	}
	if cfgCtrl.statusAddr != "127.0.0.1:18226" {
		t.Fatalf("expected statusAddr=127.0.0.1:18226, got %q", cfgCtrl.statusAddr)
	}
}

func TestSignalingIDTakenAndReconnect(t *testing.T) {
	broker := newMockPeerJSServer()
	defer broker.close()

	sc1, err := dialSignaling(context.Background(), broker.wsURL(), "peerjs", "dup-id", "tok1", nil, nil)
	if err != nil {
		t.Fatalf("first dialSignaling failed: %v", err)
	}
	defer sc1.shutdown()

	_, err = dialSignaling(context.Background(), broker.wsURL(), "peerjs", "dup-id", "tok2", nil, nil)
	if !errors.Is(err, errIDTaken) {
		t.Fatalf("expected errIDTaken on duplicate ID, got: %v", err)
	}
}

// /quit、SIGTERM 或会话结束时必须能立即打断卡住的信令握手（否则 p2ptun stop 会等
// websocket 15s 超时后强杀，留 stale pidfile）。
func TestDialSignalingHonorsContextCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	// 只接 TCP、永不回 HTTP 101: 模拟握手卡住的信令服
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				time.Sleep(30 * time.Second)
				_ = conn.Close()
			}(c)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err = dialSignaling(ctx, "ws://"+ln.Addr().String(), "peerjs", "id-cancel-test", "tok", nil, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected dial error after context cancel, got nil")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("context cancel did not interrupt handshake promptly: took %v", elapsed)
	}
}

// ---- End-to-End Integration Tests: Multi-Stream, Rapid Reconnect, Multi-Client ----

func startLocalBannerEchoServer(t *testing.T) string {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen target: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })

	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = conn.Write([]byte("SSH-2.0-MockTarget\n"))
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()
	return lis.Addr().String()
}

func TestEndToEndMultiStreamAndRapidReconnections(t *testing.T) {
	broker := newMockPeerJSServer()
	defer broker.close()
	targetAddr := startLocalBannerEchoServer(t)

	exposeStop := make(chan struct{})
	defer close(exposeStop)

	exposeCfg := config{
		mode:      "expose",
		id:        "srv-node",
		secret:    "sec123",
		target:    targetAddr,
		broker:    broker.wsURL(),
		brokers:   []string{broker.wsURL()},
		key:       "peerjs",
		stun:      []string{},
		keepalive: 2 * time.Second,
		maxPeers:  16,
	}
	mgr := newExposeManager(exposeCfg, exposeStop)
	go mgr.run()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.getSc() != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if mgr.getSc() == nil {
		t.Fatalf("expose signaling failed to come online")
	}

	for round := 1; round <= 3; round++ {
		connCh := make(chan pendingConn, 32)
		connStop := make(chan struct{})
		connectCfg := config{
			mode:      "connect",
			id:        fmt.Sprintf("cli-node-%d", round),
			peer:      "srv-node",
			secret:    "sec123",
			broker:    broker.wsURL(),
			brokers:   []string{broker.wsURL()},
			key:       "peerjs",
			stun:      []string{},
			keepalive: 2 * time.Second,
		}
		sess := newTunnelSess(connectCfg, connCh, "srv-node", connStop)
		go func() { _ = sess.run() }()

		select {
		case <-sess.trReady:
		case <-time.After(8 * time.Second):
			sess.teardown()
			close(connStop)
			t.Fatalf("round %d: datachannel failed to open within 8s", round)
		}

		var wg sync.WaitGroup
		errCh := make(chan error, 5)
		for sIdx := 0; sIdx < 5; sIdx++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				cClient, cTunnel := net.Pipe()
				defer cClient.Close()
				connCh <- pendingConn{conn: cTunnel, accepted: time.Now()}

				_ = cClient.SetDeadline(time.Now().Add(5 * time.Second))
				msg := fmt.Sprintf("hello-round-%d-stream-%d\n", round, idx)
				if _, err := cClient.Write([]byte(msg)); err != nil {
					errCh <- fmt.Errorf("write: %w", err)
					return
				}
				expected := "SSH-2.0-MockTarget\n" + msg
				buf := make([]byte, len(expected))
				if _, err := io.ReadFull(cClient, buf); err != nil {
					errCh <- fmt.Errorf("readFull: %w", err)
					return
				}
				if string(buf) != expected {
					errCh <- fmt.Errorf("got %q, want %q", string(buf), expected)
				}
			}(sIdx)
		}
		wg.Wait()
		close(errCh)
		for err := range errCh {
			if err != nil {
				t.Fatalf("round %d stream error: %v", round, err)
			}
		}

		sess.teardown()
		close(connStop)
	}
}

func TestUnifiedIDAndPeerFlags(t *testing.T) {
	// 1. Expose with -id
	cfg, err := parseConfig([]string{"-id", "tun-expo-1", "-secret", "sec1"}, "expose")
	if err != nil || cfg.id != "tun-expo-1" || !cfg.idExplicit {
		t.Fatalf("expose -id failed: cfg.id=%q, explicit=%v, err=%v", cfg.id, cfg.idExplicit, err)
	}

	// 2. Expose with -peer (interchangeable)
	cfg, err = parseConfig([]string{"-peer", "tun-expo-2", "-secret", "sec1"}, "expose")
	if err != nil || cfg.id != "tun-expo-2" || !cfg.idExplicit {
		t.Fatalf("expose -peer failed: cfg.id=%q, explicit=%v, err=%v", cfg.id, cfg.idExplicit, err)
	}

	// 3. Expose with -name
	cfg, err = parseConfig([]string{"-name", "tun-expo-3", "-secret", "sec1"}, "expose")
	if err != nil || cfg.id != "tun-expo-3" || !cfg.idExplicit {
		t.Fatalf("expose -name failed: cfg.id=%q, explicit=%v, err=%v", cfg.id, cfg.idExplicit, err)
	}

	// 4. Expose with positional arg at start
	cfg, err = parseConfig([]string{"tun-expo-4", "-secret", "sec1"}, "expose")
	if err != nil || cfg.id != "tun-expo-4" || !cfg.idExplicit {
		t.Fatalf("expose posArg start failed: cfg.id=%q, explicit=%v, err=%v", cfg.id, cfg.idExplicit, err)
	}

	// 5. Expose with positional arg at end
	cfg, err = parseConfig([]string{"-secret", "sec1", "tun-expo-5"}, "expose")
	if err != nil || cfg.id != "tun-expo-5" || !cfg.idExplicit {
		t.Fatalf("expose posArg end failed: cfg.id=%q, explicit=%v, err=%v", cfg.id, cfg.idExplicit, err)
	}

	// 6. Connect with -peer
	cfg, err = parseConfig([]string{"-peer", "target-srv-1", "-secret", "sec1"}, "connect")
	if err != nil || cfg.peer != "target-srv-1" || !strings.HasPrefix(cfg.id, "phone-") {
		t.Fatalf("connect -peer failed: cfg.peer=%q, cfg.id=%q, err=%v", cfg.peer, cfg.id, err)
	}

	// 7. Connect with -id (user's exact issue - should set target peer, NOT fail!)
	cfg, err = parseConfig([]string{"-id", "target-srv-2", "-secret", "sec1"}, "connect")
	if err != nil || cfg.peer != "target-srv-2" || !strings.HasPrefix(cfg.id, "phone-") {
		t.Fatalf("connect -id failed: cfg.peer=%q, cfg.id=%q, err=%v", cfg.peer, cfg.id, err)
	}

	// 8. Connect with -name
	cfg, err = parseConfig([]string{"-name", "target-srv-3", "-secret", "sec1"}, "connect")
	if err != nil || cfg.peer != "target-srv-3" || !strings.HasPrefix(cfg.id, "phone-") {
		t.Fatalf("connect -name failed: cfg.peer=%q, cfg.id=%q, err=%v", cfg.peer, cfg.id, err)
	}

	// 9. Connect with positional arg at start
	cfg, err = parseConfig([]string{"target-srv-4", "-secret", "sec1"}, "connect")
	if err != nil || cfg.peer != "target-srv-4" || !strings.HasPrefix(cfg.id, "phone-") {
		t.Fatalf("connect posArg start failed: cfg.peer=%q, cfg.id=%q, err=%v", cfg.peer, cfg.id, err)
	}

	// 10. Connect with positional arg at end
	cfg, err = parseConfig([]string{"-secret", "sec1", "target-srv-5"}, "connect")
	if err != nil || cfg.peer != "target-srv-5" || !strings.HasPrefix(cfg.id, "phone-") {
		t.Fatalf("connect posArg end failed: cfg.peer=%q, cfg.id=%q, err=%v", cfg.peer, cfg.id, err)
	}

	// 11. Connect with BOTH -peer (target) and -id (client ID)
	cfg, err = parseConfig([]string{"-peer", "srv-target", "-id", "my-client-laptop", "-secret", "sec1"}, "connect")
	if err != nil || cfg.peer != "srv-target" || cfg.id != "my-client-laptop" || !cfg.idExplicit {
		t.Fatalf("connect -peer & -id failed: cfg.peer=%q, cfg.id=%q, explicit=%v, err=%v", cfg.peer, cfg.id, cfg.idExplicit, err)
	}

	// 12. Connect with positional (target) and -id (client ID)
	cfg, err = parseConfig([]string{"srv-target", "-id", "my-client-laptop", "-secret", "sec1"}, "connect")
	if err != nil || cfg.peer != "srv-target" || cfg.id != "my-client-laptop" || !cfg.idExplicit {
		t.Fatalf("connect pos & -id failed: cfg.peer=%q, cfg.id=%q, explicit=%v, err=%v", cfg.peer, cfg.id, cfg.idExplicit, err)
	}

	// 13. Connect with missing peer/id -> error with helpful prompt
	_, err = parseConfig([]string{"-secret", "sec1"}, "connect")
	if err == nil || !strings.Contains(err.Error(), "请指定目标隧道名称") {
		t.Fatalf("expected helpful missing peer error, got: %v", err)
	}

	// 14. Environment variable interop test
	t.Setenv("P2PTUN_NAME", "env-tun-name")
	cfg, err = parseConfig([]string{"-secret", "sec1"}, "expose")
	if err != nil || cfg.id != "env-tun-name" {
		t.Fatalf("P2PTUN_NAME in expose failed: %v, id=%s", err, cfg.id)
	}

	cfg, err = parseConfig([]string{"-secret", "sec1"}, "connect")
	if err != nil || cfg.peer != "env-tun-name" {
		t.Fatalf("P2PTUN_NAME in connect failed: %v, peer=%s", err, cfg.peer)
	}
}
