package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

const (
	defaultBroker = "wss://0.peerjs.com"
	defaultKey    = "peerjs"
	defaultSTUN   = "stun:stun.cloudflare.com:3478,stun:stun.l.google.com:19302,stun:stun.miwifi.com:3478,stun:stun.qq.com:3478"
)

func main() {
	// never let proxy env interfere with our networking
	for _, k := range []string{
		"http_proxy", "https_proxy", "all_proxy", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
		"socks_proxy", "SOCKS_PROXY", "no_proxy", "NO_PROXY",
	} {
		os.Unsetenv(k)
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	applyNetFix() // Termux 无 resolv.conf / CA 路径修复 (netfix.go)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "expose":
		runExpose(os.Args[2:])
	case "connect":
		runConnect(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("p2ptun 0.1.1")
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `p2ptun - P2P TCP tunnel over PeerJS cloud signaling + WebRTC DataChannel
(no relay: PeerJS cloud only exchanges SDP/ICE; payload is P2P)

expose   (run where the TCP service lives, e.g. WSL):
  p2ptun expose -secret SECRET [-id MYID] [-target 127.0.0.1:22] [-stun csv]

connect  (run on the remote side, e.g. Termux):
  p2ptun connect -peer REMOTE_ID -secret SECRET [-listen 127.0.0.1:2222] [-stun csv]

common flags (env fallbacks in parens):
  -id ID      peer id on signalling server          (P2PTUN_ID)
  -secret S   pairing secret, must match both sides (P2PTUN_SECRET)
  -target H   exposed TCP endpoint (expose)         (P2PTUN_TARGET)
  -listen H   local listen addr (connect)           (P2PTUN_LISTEN)
  -broker URL signalling server                     (P2PTUN_BROKER=%s)
  -key K      PeerJS api key                        (P2PTUN_KEY=peerjs)
  -stun csv   STUN servers, comma separated         (P2PTUN_STUN)

example:
  expose  : p2ptun expose -id wslssh-a1b2 -secret hunter2 -target 127.0.0.1:22
  connect : p2ptun connect -peer wslssh-a1b2 -secret hunter2 -listen 127.0.0.1:2222
  then    : ssh -p 2222 user@127.0.0.1
`, defaultBroker)
}

type config struct {
	mode   string
	id     string
	peer   string
	secret string
	target string
	listen string
	broker string
	key    string
	stun   []string
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseConfig(args []string, mode string) (config, error) {
	fs := flag.NewFlagSet(mode, flag.ContinueOnError)
	cfg := config{
		mode:   mode,
		id:     envOr("P2PTUN_ID", ""),
		peer:   envOr("P2PTUN_PEER", ""),
		secret: envOr("P2PTUN_SECRET", ""),
		target: envOr("P2PTUN_TARGET", "127.0.0.1:22"),
		listen: envOr("P2PTUN_LISTEN", "127.0.0.1:2222"),
		broker: envOr("P2PTUN_BROKER", defaultBroker),
		key:    envOr("P2PTUN_KEY", defaultKey),
		stun:   splitCSV(envOr("P2PTUN_STUN", defaultSTUN)),
	}
	fs.StringVar(&cfg.id, "id", cfg.id, "peer id")
	fs.StringVar(&cfg.peer, "peer", cfg.peer, "remote peer id (connect)")
	fs.StringVar(&cfg.secret, "secret", cfg.secret, "pairing secret")
	fs.StringVar(&cfg.target, "target", cfg.target, "target tcp endpoint (expose)")
	fs.StringVar(&cfg.listen, "listen", cfg.listen, "local listen addr (connect)")
	fs.StringVar(&cfg.broker, "broker", cfg.broker, "signaling broker URL")
	fs.StringVar(&cfg.key, "key", cfg.key, "PeerJS api key")
	stun := ""
	fs.StringVar(&stun, "stun", "", "STUN servers, comma separated")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}

	if stun != "" {
		cfg.stun = splitCSV(stun)
	}
	if cfg.id == "" {
		cfg.id = map[string]string{"expose": "wslssh", "connect": "phone"}[mode] + "-" + randHex(4)
	}
	if mode == "connect" && cfg.peer == "" {
		return cfg, fmt.Errorf("connect: -peer REMOTE_ID is required")
	}
	if cfg.secret == "" {
		return cfg, fmt.Errorf("%s: -secret is required", mode)
	}
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

func (c config) iceServers() []webrtc.ICEServer {
	urls := c.stun
	if len(urls) == 0 {
		urls = splitCSV(defaultSTUN)
	}
	return []webrtc.ICEServer{{URLs: urls}}
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// ---- shared SDP/ICE candidate state (buffers candidates until remote desc is set) ----

type sdpState struct {
	mu        sync.Mutex
	remoteSet bool
	pending   []webrtc.ICECandidateInit
}

func (s *sdpState) onCandidate(pc *webrtc.PeerConnection, init webrtc.ICECandidateInit) {
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
		init.SDPMid = &c.SDPMid
	}
	ml := c.SDPMLineIndex
	init.SDPMLineIndex = &ml
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

// ---------------- expose (runs next to the TCP service) ----------------

func runExpose(args []string) {
	cfg, err := parseConfig(args, "expose")
	if err != nil {
		log.Fatalf("%v", err)
	}
	log.Printf("expose: id=%s target=%s broker=%s secret=***", cfg.id, cfg.target, cfg.broker)
	for {
		if err := exposeOnce(cfg); err != nil {
			log.Printf("expose: session ended: %v (retry in 3s)", err)
		}
		time.Sleep(3 * time.Second)
	}
}

type exposeCtx struct {
	mu     sync.Mutex
	sc     *signalingClient
	peer   string // destination peer id (the connect side)
	connID string
}

func (x *exposeCtx) set(sc *signalingClient, peer, connID string) {
	x.mu.Lock()
	x.sc = sc
	x.peer = peer
	x.connID = connID
	x.mu.Unlock()
}

func (x *exposeCtx) candidate() {
	x.mu.Lock()
	sc, peer, connID := x.sc, x.peer, x.connID
	x.mu.Unlock()
	if sc == nil || peer == "" {
		return
	}
	_ = sc.sendSignal(pjCandidate, peer, pjSignal{
		Type:         "data",
		ConnectionID: connID,
	})
}

func exposeOnce(cfg config) error {
	token := randHex(12)
	cancel := make(chan struct{})
	var once sync.Once
	cancelFn := func() { once.Do(func() { close(cancel) }) }

	reg := newStreamReg()
	ctx := &exposeCtx{}
	sdp := &sdpState{}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: cfg.iceServers()})
	if err != nil {
		return err
	}
	defer pc.Close()

	var trMu sync.Mutex
	var tr *transport

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnOpen(func() {
			log.Printf("expose: datachannel open")
			t := newTransport(dc, func(typ uint8, id uint32, payload []byte) {
				trMu.Lock()
				cur := tr
				trMu.Unlock()
				if cur != nil {
					handleFrameExpose(cfg, reg, cur, typ, id, payload)
				}
			})
			t.setOnClose(cancelFn)
			trMu.Lock()
			tr = t
			trMu.Unlock()
		})
	})

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		ctx.mu.Lock()
		sc, peer, connID := ctx.sc, ctx.peer, ctx.connID
		ctx.mu.Unlock()
		if sc == nil || peer == "" {
			return
		}
		_ = sc.sendSignal(pjCandidate, peer, pjSignal{
			Type:         "data",
			ConnectionID: connID,
			Candidate:    candToPJ(c.ToJSON()),
		})
	})

	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		log.Printf("expose: pc state=%s", st)
		if st == webrtc.PeerConnectionStateFailed || st == webrtc.PeerConnectionStateClosed {
			cancelFn()
		}
	})

	var sc *signalingClient
	sc, err = dialSignaling(cfg.broker, cfg.key, cfg.id, token, func(typ, src string, sig pjSignal) {
		handleSignalExpose(cfg, pc, ctx, sdp, typ, src, sig, sc)
	}, func(err error) {
		log.Printf("expose: signaling closed: %v", err)
		cancelFn()
	})
	if err != nil {
		return err
	}
	defer sc.shutdown()

	log.Printf("expose: signaling ready id=%s", cfg.id)
	log.Printf("expose: connect side command: p2ptun connect -peer %s -secret %s -listen 127.0.0.1:2222",
		cfg.id, cfg.secret)

	<-cancel
	reg.closeAll()
	return fmt.Errorf("session over")
}

func handleSignalExpose(cfg config, pc *webrtc.PeerConnection, ctx *exposeCtx, sdp *sdpState, typ, src string, sig pjSignal, sc *signalingClient) {
	switch typ {
	case pjOffer:
		if sig.Metadata != cfg.secret {
			log.Printf("expose: REJECT offer from %s: bad secret", src)
			return
		}
		log.Printf("expose: OFFER from %s conn=%s", src, sig.ConnectionID)
		ctx.set(sc, src, sig.ConnectionID)

		if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sig.SDP.SDP}); err != nil {
			log.Printf("expose: setRemoteDescription: %v", err)
			return
		}
		answer, err := pc.CreateAnswer(nil)
		if err != nil {
			log.Printf("expose: createAnswer: %v", err)
			return
		}
		if err := pc.SetLocalDescription(answer); err != nil {
			log.Printf("expose: setLocalDescription: %v", err)
			return
		}
		if err := sc.sendSignal(pjAnswer, src, pjSignal{
			Type:         "data",
			ConnectionID: sig.ConnectionID,
			SDP:          pjSDP{Type: "answer", SDP: answer.SDP},
		}); err != nil {
			log.Printf("expose: send ANSWER: %v", err)
		}
		sdp.onRemoteSet(pc)
	case pjCandidate:
		sdp.onCandidate(pc, pjToCand(sig.Candidate))
	}
}

func handleFrameExpose(cfg config, reg *streamReg, tr *transport, typ uint8, id uint32, payload []byte) {
	switch typ {
	case frameSyn:
		st := &stream{id: id, ch: make(chan []byte, 1024), stopped: make(chan struct{})}
		reg.add(st)
		go func() {
			conn, err := net.DialTimeout("tcp", cfg.target, 5*time.Second)
			if err != nil {
				log.Printf("expose: dial %s: %v", cfg.target, err)
				_ = tr.send(frameFin, id, nil)
				st.stop()
				reg.remove(id)
				return
			}
			st.conn = conn
			go connToDC(conn, id, func(tt uint8, p []byte) error { return tr.send(tt, id, p) })
			go dcToConn(conn, st.ch, nil)
		}()
	case frameData:
		st := reg.get(id)
		if st == nil {
			return
		}
		select {
		case st.ch <- payload:
		case <-st.stopped:
		}
	case frameFin:
		if st := reg.get(id); st != nil {
			st.stop()
			reg.remove(id)
		}
	}
}

// ---------------- connect (runs on the remote side, e.g. Termux) ----------------

func runConnect(args []string) {
	cfg, err := parseConfig(args, "connect")
	if err != nil {
		log.Fatalf("%v", err)
	}
	log.Printf("connect: id=%s peer=%s listen=%s broker=%s secret=***",
		cfg.id, cfg.peer, cfg.listen, cfg.broker)

	lis, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		log.Fatalf("connect: listen %s: %v", cfg.listen, err)
	}
	log.Printf("connect: listening on %s (ssh -p %s user@127.0.0.1)", cfg.listen, portOf(cfg.listen))

	connCh := make(chan net.Conn, 128)
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				log.Printf("connect: accept: %v", err)
				return
			}
			select {
			case connCh <- c:
			default:
				c.Close()
			}
		}
	}()

	for {
		sess := newTunnelSess(cfg, connCh, cfg.peer)
		err := sess.run()
		sess.teardown()
		log.Printf("connect: session ended: %v (retry in 3s)", err)
		time.Sleep(3 * time.Second)
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
	cfg     config
	connCh  chan net.Conn
	peer    string
	token   string
	pc      *webrtc.PeerConnection
	scMu    sync.Mutex
	sc      *signalingClient
	tr      *transport
	trReady chan struct{}
	reg     *streamReg
	cancel  chan struct{}
	once    sync.Once
}

func newTunnelSess(cfg config, connCh chan net.Conn, peer string) *tunnelSess {
	return &tunnelSess{
		cfg:     cfg,
		connCh:  connCh,
		peer:    peer,
		token:   randHex(12),
		trReady: make(chan struct{}),
		reg:     newStreamReg(),
		cancel:  make(chan struct{}),
	}
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
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: s.cfg.iceServers()})
	if err != nil {
		return err
	}
	s.pc = pc
	sdp := &sdpState{}

	dc, err := pc.CreateDataChannel("p2ptun", &webrtc.DataChannelInit{Ordered: boolPtr(true)})
	if err != nil {
		return err
	}
	var trMu sync.Mutex
	dc.OnOpen(func() {
		log.Printf("connect: datachannel open")
		t := newTransport(dc, func(typ uint8, id uint32, payload []byte) {
			trMu.Lock()
			cur := s.tr
			trMu.Unlock()
			if cur != nil {
				handleFrameConnect(s.reg, cur, typ, id, payload)
			}
		})
		t.setOnClose(s.cancelFn)
		trMu.Lock()
		s.tr = t
		trMu.Unlock()
		close(s.trReady)
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
			ConnectionID: s.peer + "-main",
			Candidate:    candToPJ(c.ToJSON()),
		})
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		log.Printf("connect: pc state=%s", st)
		if st == webrtc.PeerConnectionStateFailed || st == webrtc.PeerConnectionStateClosed {
			s.cancelFn()
		}
	})

	sc, err := dialSignaling(s.cfg.broker, s.cfg.key, s.cfg.id, s.token, func(typ, src string, sig pjSignal) {
		s.onSignal(typ, src, sig, sdp)
	}, func(err error) {
		log.Printf("connect: signaling closed: %v", err)
		s.cancelFn()
	})
	if err != nil {
		return err
	}
	s.sc = sc
	s.setSc(sc)
	defer sc.shutdown()

	log.Printf("connect: signaling ready, sending OFFER to %s", s.peer)
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
		ConnectionID:  s.peer + "-main",
		Label:         "p2ptun",
		Metadata:      s.cfg.secret,
		Reliable:      &reliable,
		Serialization: "default",
	}); err != nil {
		return err
	}

	go s.acceptLoop()
	<-s.cancel
	return fmt.Errorf("session over")
}

func (s *tunnelSess) onSignal(typ, src string, sig pjSignal, sdp *sdpState) {
	switch typ {
	case pjAnswer:
		if sdp.remoteLocked() {
			return
		}
		log.Printf("connect: ANSWER from %s", src)
		if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sig.SDP.SDP}); err != nil {
			log.Printf("connect: setRemoteDescription: %v", err)
			s.cancelFn()
			return
		}
		sdp.onRemoteSet(s.pc)
	case pjCandidate:
		sdp.onCandidate(s.pc, pjToCand(sig.Candidate))
	}
}

func (s *tunnelSess) acceptLoop() {
	for {
		select {
		case <-s.cancel:
			return
		case c := <-s.connCh:
			if c == nil {
				return
			}
			s.registerConn(c)
		}
	}
}

func (s *tunnelSess) registerConn(c net.Conn) {
	select {
	case <-s.trReady:
	case <-s.cancel:
		c.Close()
		return
	}
	id := s.reg.allocID()
	st := &stream{id: id, conn: c, ch: make(chan []byte, 1024), stopped: make(chan struct{})}
	s.reg.add(st)
	go connToDC(c, id, func(tt uint8, p []byte) error { return s.tr.send(tt, id, p) })
	go dcToConn(c, st.ch, nil)
	if err := s.tr.send(frameSyn, id, nil); err != nil {
		log.Printf("connect: SYN failed: %v", err)
		st.stop()
		s.reg.remove(id)
	}
}

func (s *tunnelSess) teardown() {
	s.cancelFn()
	if s.pc != nil {
		s.pc.Close()
	}
	if s.sc != nil {
		s.sc.shutdown()
	}
	s.reg.closeAll()
}

func handleFrameConnect(reg *streamReg, tr *transport, typ uint8, id uint32, payload []byte) {
	switch typ {
	case frameData:
		st := reg.get(id)
		if st == nil {
			return
		}
		select {
		case st.ch <- payload:
		case <-st.stopped:
		}
	case frameFin:
		if st := reg.get(id); st != nil {
			st.stop()
			reg.remove(id)
		}
	}
}

func boolPtr(b bool) *bool { return &b }