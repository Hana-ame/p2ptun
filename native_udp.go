package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	hyClient "github.com/apernet/hysteria/core/v2/client"
	hyServer "github.com/apernet/hysteria/core/v2/server"
	"github.com/pion/ice/v4"
	"github.com/pion/stun/v3"
	"github.com/pion/webrtc/v4"
	quic "github.com/quic-go/quic-go"
)

const (
	controlDataChannelLabel = "p2ptun-control"
	nativeQUICSynByte       = 0x01
)

var (
	globalECDSACert    tls.Certificate
	globalECDSACertErr error
)

func init() {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		globalECDSACertErr = err
		return
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "p2ptun-native-udp"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		globalECDSACertErr = err
		return
	}
	globalECDSACert = tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  priv,
	}
}

// normalizeProto 统一规范化协议名称:
//   - "hy2" / "hysteria2" / "hysteria" -> "hy2" (默认: 原生 ICE UDP + Hysteria 2 Brutal CC)
//   - "quic" -> "quic" (原生 ICE UDP + QUIC)
//   - "dc-quic" -> "dc-quic" (WebRTC DataChannel 承载 QUIC)
//   - "classic" / "sctp" / "dc" -> "classic" (WebRTC DataChannel 多路复用帧)
func normalizeProto(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "hy2", "hysteria2", "hysteria":
		return "hy2"
	case "quic":
		return "quic"
	case "dc-quic", "dcquic":
		return "dc-quic"
	case "classic", "sctp", "dc", "datachannel":
		return "classic"
	default:
		return "hy2"
	}
}

// icePacketConn wraps *ice.Conn into net.PacketConn for QUIC and Hysteria 2
type icePacketConn struct {
	conn *ice.Conn
}

func (c *icePacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, err := c.conn.Read(p)
	if err != nil {
		return 0, nil, err
	}
	globalStats.bytesRx.Add(uint64(n))
	return n, c.conn.RemoteAddr(), nil
}

func (c *icePacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	n, err := c.conn.Write(p)
	if n > 0 {
		globalStats.bytesTx.Add(uint64(n))
	}
	return n, err
}

func (c *icePacketConn) Close() error {
	return c.conn.Close()
}

func (c *icePacketConn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

func (c *icePacketConn) SetDeadline(t time.Time) error {
	return c.conn.SetDeadline(t)
}

func (c *icePacketConn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

func (c *icePacketConn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}

// udpScheduleMsg is exchanged over the SCTP DataChannel (调度面)
type udpScheduleMsg struct {
	Type      string `json:"type"`                // "init", "cand", "cand_end", "ready", "fallback"
	Proto     string `json:"proto,omitempty"`     // "hy2" or "quic"
	Bandwidth int    `json:"bandwidth,omitempty"` // Mbps (for Brutal CC)
	Ufrag     string `json:"ufrag,omitempty"`
	Pwd       string `json:"pwd,omitempty"`
	Cand      string `json:"cand,omitempty"`
	Error     string `json:"error,omitempty"`
}

func parseStunURIs(stuns []string) []*stun.URI {
	var uris []*stun.URI
	for _, raw := range stuns {
		if u, err := stun.ParseURI(raw); err == nil {
			uris = append(uris, u)
		}
	}
	return uris
}

// buildICEAgent creates an ice.Agent configured with STUN servers, optional port range, and 1:1 NAT IPs
func buildICEAgent(cfg config) (*ice.Agent, error) {
	uris := parseStunURIs(cfg.stun)
	agentConfig := &ice.AgentConfig{
		Urls:            uris,
		NetworkTypes:    []ice.NetworkType{ice.NetworkTypeUDP4, ice.NetworkTypeUDP6},
		IncludeLoopback: true,
	}
	if cfg.icePorts != "" {
		parts := strings.Split(cfg.icePorts, "-")
		if len(parts) == 2 {
			minP, err1 := strconv.ParseUint(strings.TrimSpace(parts[0]), 10, 16)
			maxP, err2 := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 16)
			if err1 == nil && err2 == nil && minP > 0 && maxP >= minP {
				agentConfig.PortMin = uint16(minP)
				agentConfig.PortMax = uint16(maxP)
			}
		}
	}
	if len(cfg.nat1to1) > 0 {
		agentConfig.NAT1To1IPs = cfg.nat1to1
		agentConfig.NAT1To1IPCandidateType = ice.CandidateTypeHost
	}
	return ice.NewAgent(agentConfig)
}

// wireControlChannel 立即在 DataChannel 上挂载消息缓冲队列与 Open 状态通知，
// 防止对端在 OnOpen 瞬间发出的 "init" 或 "cand" 消息因协程启动延迟而丢失。
func wireControlChannel(dc *webrtc.DataChannel, onOpenExtra ...func()) (<-chan webrtc.DataChannelMessage, <-chan struct{}) {
	msgCh := make(chan webrtc.DataChannelMessage, 64)
	openCh := make(chan struct{})
	var openOnce sync.Once
	markOpen := func() {
		openOnce.Do(func() {
			close(openCh)
			for _, fn := range onOpenExtra {
				if fn != nil {
					fn()
				}
			}
		})
	}

	if dc.ReadyState() == webrtc.DataChannelStateOpen {
		markOpen()
	}
	dc.OnOpen(func() {
		markOpen()
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		cp := make([]byte, len(msg.Data))
		copy(cp, msg.Data)
		select {
		case msgCh <- webrtc.DataChannelMessage{Data: cp, IsString: msg.IsString}:
		default:
		}
	})
	return msgCh, openCh
}

// runNativeUDPExpose handles the expose side: coordinated via SCTP DataChannel, connects native UDP.
func runNativeUDPExpose(cfg config, dc *webrtc.DataChannel, cancelFn func()) {
	msgCh, openCh := wireControlChannel(dc)
	runNativeUDPExposeWithChan(context.Background(), cfg, dc, cancelFn, msgCh, openCh)
}

func runNativeUDPExposeWithChan(sessCtx context.Context, cfg config, dc *webrtc.DataChannel, cancelFn func(), msgCh <-chan webrtc.DataChannelMessage, openCh <-chan struct{}) {
	if cancelFn != nil {
		defer cancelFn()
	}
	if sessCtx == nil {
		sessCtx = context.Background()
	}

	log.Printf("expose: control channel ready (p2ptun-control), negotiating native UDP hole punch...")

	agent, err := buildICEAgent(cfg)
	if err != nil {
		log.Printf("expose: failed to build ICE agent: %v", err)
		return
	}
	defer agent.Close()

	ufragLocal, pwdLocal, err := agent.GetLocalUserCredentials()
	if err != nil {
		log.Printf("expose: get local ICE creds: %v", err)
		return
	}

	sendCtrl := func(msg udpScheduleMsg) {
		if dc.ReadyState() != webrtc.DataChannelStateOpen {
			select {
			case <-openCh:
			case <-sessCtx.Done():
				return
			case <-time.After(5 * time.Second):
				return
			}
		}
		data, _ := json.Marshal(msg)
		_ = dc.Send(data)
	}

	var remoteUfrag, remotePwd string
	protoChoice := normalizeProto(cfg.proto)
	bwMbps := cfg.bandwidthMbps
	if bwMbps <= 0 {
		bwMbps = 50
	}

	readyCh := make(chan struct{})
	var onceReady sync.Once

	_ = agent.OnCandidate(func(c ice.Candidate) {
		if c == nil {
			sendCtrl(udpScheduleMsg{Type: "cand_end"})
			return
		}
		sendCtrl(udpScheduleMsg{Type: "cand", Cand: c.Marshal()})
	})

	go func() {
		for {
			select {
			case <-sessCtx.Done():
				return
			case msg, ok := <-msgCh:
				if !ok {
					return
				}
				var sm udpScheduleMsg
				if err := json.Unmarshal(msg.Data, &sm); err != nil {
					continue
				}
				switch sm.Type {
				case "init":
					remoteUfrag = sm.Ufrag
					remotePwd = sm.Pwd
					if sm.Proto != "" {
						protoChoice = normalizeProto(sm.Proto)
					}
					if sm.Bandwidth > 0 {
						bwMbps = sm.Bandwidth
					}
					sendCtrl(udpScheduleMsg{
						Type:      "init",
						Proto:     protoChoice,
						Bandwidth: bwMbps,
						Ufrag:     ufragLocal,
						Pwd:       pwdLocal,
					})
					_ = agent.GatherCandidates()
					onceReady.Do(func() { close(readyCh) })
				case "cand":
					if parsed, err := ice.UnmarshalCandidate(sm.Cand); err == nil {
						_ = agent.AddRemoteCandidate(parsed)
					}
				case "cand_end":
				}
			}
		}
	}()

	ctx, cancel := context.WithTimeout(sessCtx, 15*time.Second)
	defer cancel()

	select {
	case <-ctx.Done():
		log.Printf("expose: timed out waiting for init on SCTP control channel")
		return
	case <-readyCh:
	}

	log.Printf("expose: punching native UDP to client (proto: %s, bw: %d Mbps)...", protoChoice, bwMbps)
	iceConn, err := agent.Accept(ctx, remoteUfrag, remotePwd)
	if err != nil {
		log.Printf("expose: native UDP hole punch failed (accept): %v", err)
		return
	}
	defer iceConn.Close()

	remoteStr := ""
	if iceConn.RemoteAddr() != nil {
		remoteStr = iceConn.RemoteAddr().String()
	}
	globalStats.setTransport(protoChoice, remoteStr)
	log.Printf("expose: native UDP direct tunnel established: %s <===> %s", iceConn.LocalAddr(), iceConn.RemoteAddr())
	pConn := &icePacketConn{conn: iceConn}

	if protoChoice == "quic" {
		runNativeQUICExpose(sessCtx, cfg, pConn)
	} else {
		runNativeHy2Expose(sessCtx, cfg, pConn, bwMbps)
	}
}

// runNativeUDPConnect handles the connect side: coordinated via SCTP DataChannel, connects native UDP.
func runNativeUDPConnect(s *tunnelSess, dc *webrtc.DataChannel) {
	msgCh, openCh := wireControlChannel(dc)
	runNativeUDPConnectWithChan(s, dc, msgCh, openCh)
}

func runNativeUDPConnectWithChan(s *tunnelSess, dc *webrtc.DataChannel, msgCh <-chan webrtc.DataChannelMessage, openCh <-chan struct{}) {
	log.Printf("connect: control channel ready (p2ptun-control), negotiating native UDP hole punch...")

	sessCtx := s.ctx
	if sessCtx == nil {
		sessCtx = context.Background()
	}

	agent, err := buildICEAgent(s.cfg)
	if err != nil {
		log.Printf("connect: failed to build ICE agent: %v", err)
		s.cancelFn()
		return
	}
	defer agent.Close()

	ufragLocal, pwdLocal, err := agent.GetLocalUserCredentials()
	if err != nil {
		log.Printf("connect: get local ICE creds: %v", err)
		s.cancelFn()
		return
	}

	sendCtrl := func(msg udpScheduleMsg) {
		if dc.ReadyState() != webrtc.DataChannelStateOpen {
			select {
			case <-openCh:
			case <-sessCtx.Done():
				return
			case <-s.cancel:
				return
			case <-time.After(5 * time.Second):
				return
			}
		}
		data, _ := json.Marshal(msg)
		_ = dc.Send(data)
	}

	protoChoice := normalizeProto(s.cfg.proto)
	bwMbps := s.cfg.bandwidthMbps
	if bwMbps <= 0 {
		bwMbps = 50
	}

	var remoteUfrag, remotePwd string
	readyCh := make(chan struct{})
	var onceReady sync.Once

	_ = agent.OnCandidate(func(c ice.Candidate) {
		if c == nil {
			sendCtrl(udpScheduleMsg{Type: "cand_end"})
			return
		}
		sendCtrl(udpScheduleMsg{Type: "cand", Cand: c.Marshal()})
	})

	go func() {
		for {
			select {
			case <-s.cancel:
				return
			case <-sessCtx.Done():
				return
			case msg, ok := <-msgCh:
				if !ok {
					return
				}
				var sm udpScheduleMsg
				if err := json.Unmarshal(msg.Data, &sm); err != nil {
					continue
				}
				switch sm.Type {
				case "init":
					remoteUfrag = sm.Ufrag
					remotePwd = sm.Pwd
					onceReady.Do(func() { close(readyCh) })
				case "cand":
					if parsed, err := ice.UnmarshalCandidate(sm.Cand); err == nil {
						_ = agent.AddRemoteCandidate(parsed)
					}
				}
			}
		}
	}()

	sendCtrl(udpScheduleMsg{
		Type:      "init",
		Proto:     protoChoice,
		Bandwidth: bwMbps,
		Ufrag:     ufragLocal,
		Pwd:       pwdLocal,
	})
	_ = agent.GatherCandidates()

	ctx, cancel := context.WithTimeout(sessCtx, 15*time.Second)
	defer cancel()

	select {
	case <-ctx.Done():
		log.Printf("connect: timed out waiting for init from expose on SCTP control channel")
		s.cancelFn()
		return
	case <-s.cancel:
		return
	case <-readyCh:
	}

	log.Printf("connect: punching native UDP to server (proto: %s, bw: %d Mbps)...", protoChoice, bwMbps)
	iceConn, err := agent.Dial(ctx, remoteUfrag, remotePwd)
	if err != nil {
		log.Printf("connect: native UDP hole punch failed (dial): %v", err)
		s.cancelFn()
		return
	}
	defer iceConn.Close()

	remoteStr := ""
	if iceConn.RemoteAddr() != nil {
		remoteStr = iceConn.RemoteAddr().String()
	}
	globalStats.setTransport(protoChoice, remoteStr)
	log.Printf("connect: native UDP direct tunnel established: %s <===> %s", iceConn.LocalAddr(), iceConn.RemoteAddr())
	pConn := &icePacketConn{conn: iceConn}

	if protoChoice == "quic" {
		runNativeQUICConnect(s, pConn)
	} else {
		runNativeHy2Connect(s, pConn, bwMbps)
	}
}

// runNativeHy2Expose runs Hysteria 2 server on top of bare UDP packet conn
func runNativeHy2Expose(ctx context.Context, cfg config, pConn net.PacketConn, bwMbps int) {
	if globalECDSACertErr != nil {
		log.Printf("expose: ecdsa cert error: %v", globalECDSACertErr)
		return
	}

	bwBytesPerSec := uint64(bwMbps) * 1024 * 1024 / 8
	srv, err := hyServer.NewServer(&hyServer.Config{
		Conn: pConn,
		TLSConfig: hyServer.TLSConfig{
			Certificates: []tls.Certificate{globalECDSACert},
		},
		Authenticator: &hy2Auth{secret: cfg.secret},
		Outbound:      &hy2Outbound{target: cfg.target, ctx: ctx},
		BandwidthConfig: hyServer.BandwidthConfig{
			MaxTx: bwBytesPerSec,
			MaxRx: bwBytesPerSec,
		},
		DisableUDP: true,
	})
	if err != nil {
		log.Printf("expose: hy2 server error: %v", err)
		return
	}
	defer srv.Close()

	if ctx != nil {
		go func() {
			<-ctx.Done()
			_ = srv.Close()
		}()
	}

	log.Printf("expose: native Hysteria 2 server active (Brutal CC: %d Mbps)", bwMbps)
	_ = srv.Serve()
}

// runNativeHy2Connect runs Hysteria 2 client on top of bare UDP packet conn
func runNativeHy2Connect(s *tunnelSess, pConn net.PacketConn, bwMbps int) {
	bwBytesPerSec := uint64(bwMbps) * 1024 * 1024 / 8

	cli, info, err := hyClient.NewClient(&hyClient.Config{
		ConnFactory: &hy2ConnFactory{conn: pConn},
		ServerAddr:  pConn.LocalAddr(),
		Auth:        s.cfg.secret,
		TLSConfig: hyClient.TLSConfig{
			InsecureSkipVerify: true,
		},
		BandwidthConfig: hyClient.BandwidthConfig{
			MaxTx: bwBytesPerSec,
			MaxRx: bwBytesPerSec,
		},
		FastOpen: true,
	})
	if err != nil {
		log.Printf("connect: native Hysteria 2 dial error: %v", err)
		s.cancelFn()
		return
	}
	log.Printf("connect: native Hysteria 2 handshake success (Brutal CC: %d Mbps, tx=%d bps)", bwMbps, info.Tx)
	if s.cfg.listen != "" {
		tipPort := portOf(s.cfg.listen)
		if tipPort == "22" || tipPort == "2222" {
			log.Printf("connect: 提示 (tunnel ready) ➔ 本地监听 %s 已就绪，连接命令: ssh -p %s user@127.0.0.1", s.cfg.listen, tipPort)
		} else {
			log.Printf("connect: 提示 (tunnel ready) ➔ 本地监听 %s 已就绪，测试代理命令: curl -x http://127.0.0.1:%s -I https://www.google.com", s.cfg.listen, tipPort)
		}
	}

	s.hy2Mu.Lock()
	s.hy2Client = cli
	s.hy2Mu.Unlock()
	s.wasReady.Store(true)
	s.lastActive.Store(time.Now().UnixNano())
	globalStats.activePeers.Store(1)
	s.readyOnce.Do(func() { close(s.trReady) })

	<-s.cancel
	_ = cli.Close()
	s.cancelFn()
}

// runNativeQUICExpose runs quic-go server on bare UDP packet conn
func runNativeQUICExpose(ctx context.Context, cfg config, pConn net.PacketConn) {
	if globalECDSACertErr != nil {
		log.Printf("expose: ecdsa cert error: %v", globalECDSACertErr)
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	alpn := "p2ptun-raw-quic"
	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{globalECDSACert},
		NextProtos:   []string{alpn},
	}
	quicConf := &quic.Config{
		KeepAlivePeriod:                5 * time.Second,
		MaxIdleTimeout:                 45 * time.Second,
		MaxIncomingStreams:             1024,
		InitialStreamReceiveWindow:     4 << 20,
		MaxStreamReceiveWindow:         16 << 20,
		InitialConnectionReceiveWindow: 8 << 20,
		MaxConnectionReceiveWindow:     32 << 20,
		EnableDatagrams:                true,
	}

	tr := &quic.Transport{Conn: pConn}
	defer tr.Close()

	listener, err := tr.Listen(tlsConf, quicConf)
	if err != nil {
		log.Printf("expose: quic listen: %v", err)
		return
	}
	defer listener.Close()

	log.Printf("expose: native QUIC server active (ALPN=%s)", alpn)

	qConn, err := listener.Accept(ctx)
	if err != nil {
		log.Printf("expose: quic accept: %v", err)
		return
	}
	defer qConn.CloseWithError(0, "bye")

	go func(conn *quic.Conn) {
		for {
			msg, err := conn.ReceiveDatagram(ctx)
			if err != nil {
				return
			}
			_ = conn.SendDatagram(msg)
		}
	}(qConn)

	for {
		stream, err := qConn.AcceptStream(ctx)
		if err != nil {
			return
		}
		globalStats.activeStreams.Add(1)
		go func(qs *quic.Stream) {
			defer globalStats.activeStreams.Add(-1)
			defer qs.Close()
			dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			dest, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", cfg.target)
			cancel()
			if err != nil {
				log.Printf("expose: dial target %s: %v", cfg.target, err)
				return
			}
			defer dest.Close()
			tuneTCPConn(dest)
			pipeBidirectional(dest, qs, nil)
		}(stream)
	}
}

// runNativeQUICConnect runs quic-go client on bare UDP packet conn
func runNativeQUICConnect(s *tunnelSess, pConn net.PacketConn) {
	sessCtx := s.ctx
	if sessCtx == nil {
		sessCtx = context.Background()
	}
	alpn := "p2ptun-raw-quic"
	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{alpn},
	}
	quicConf := &quic.Config{
		KeepAlivePeriod:                5 * time.Second,
		MaxIdleTimeout:                 45 * time.Second,
		MaxIncomingStreams:             1024,
		InitialStreamReceiveWindow:     4 << 20,
		MaxStreamReceiveWindow:         16 << 20,
		InitialConnectionReceiveWindow: 8 << 20,
		MaxConnectionReceiveWindow:     32 << 20,
		EnableDatagrams:                true,
	}

	tr := &quic.Transport{Conn: pConn}
	defer tr.Close()

	qConn, err := tr.Dial(sessCtx, pConn.LocalAddr(), tlsConf, quicConf)
	if err != nil {
		log.Printf("connect: native QUIC dial: %v", err)
		s.cancelFn()
		return
	}
	defer qConn.CloseWithError(0, "bye")

	log.Printf("connect: native QUIC handshake success (ALPN=%s)", alpn)
	if s.cfg.listen != "" {
		tipPort := portOf(s.cfg.listen)
		if tipPort == "22" || tipPort == "2222" {
			log.Printf("connect: 提示 (tunnel ready) ➔ 本地监听 %s 已就绪，连接命令: ssh -p %s user@127.0.0.1", s.cfg.listen, tipPort)
		} else {
			log.Printf("connect: 提示 (tunnel ready) ➔ 本地监听 %s 已就绪，测试代理命令: curl -x http://127.0.0.1:%s -I https://www.google.com", s.cfg.listen, tipPort)
		}
	}

	s.quicMu.Lock()
	s.quicConn = qConn
	s.quicMu.Unlock()
	s.wasReady.Store(true)
	s.lastActive.Store(time.Now().UnixNano())
	globalStats.activePeers.Store(1)
	s.readyOnce.Do(func() { close(s.trReady) })

	// RFC 9221 datagram RTT probe
	go func() {
		interval := s.cfg.keepalive
		if interval <= 0 {
			interval = 5 * time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.cancel:
				return
			case <-ticker.C:
				var ts [8]byte
				binary.BigEndian.PutUint64(ts[:], uint64(time.Now().UnixNano()))
				_ = qConn.SendDatagram(ts[:])
			}
		}
	}()

	go func() {
		for {
			msg, err := qConn.ReceiveDatagram(sessCtx)
			if err != nil {
				return
			}
			if len(msg) >= 8 {
				sent := int64(binary.BigEndian.Uint64(msg[:8]))
				rtt := time.Now().UnixNano() - sent
				if rtt > 0 && rtt < int64(time.Minute) {
					globalStats.lastRTTMs.Store(rtt / int64(time.Millisecond))
				}
			}
		}
	}()

	select {
	case <-s.cancel:
	case <-qConn.Context().Done():
	}
	s.cancelFn()
}

type hy2ConnFactory struct {
	conn net.PacketConn
}

func (f *hy2ConnFactory) New(net.Addr) (net.PacketConn, error) {
	return f.conn, nil
}

type hy2Auth struct {
	secret string
}

func (a *hy2Auth) Authenticate(addr net.Addr, auth string, tx uint64) (bool, string) {
	if auth == a.secret {
		return true, "p2ptun-user"
	}
	return false, ""
}

type hy2Outbound struct {
	target string
	ctx    context.Context
}

func (o *hy2Outbound) TCP(reqAddr string) (net.Conn, error) {
	dialCtx := o.ctx
	if dialCtx == nil {
		dialCtx = context.Background()
	}
	dctx, cancel := context.WithTimeout(dialCtx, 5*time.Second)
	defer cancel()
	c, err := (&net.Dialer{}).DialContext(dctx, "tcp", o.target)
	if err != nil {
		return nil, err
	}
	tuneTCPConn(c)
	return c, nil
}

func (o *hy2Outbound) UDP(reqAddr string) (hyServer.UDPConn, error) {
	return nil, errors.New("udp unsupported")
}

func (o *hy2Outbound) CheckUDP(reqAddr string) error {
	return errors.New("udp unsupported")
}

func pipeBidirectional(c1 net.Conn, c2 io.ReadWriteCloser, onActivity func()) {
	var wg sync.WaitGroup
	wg.Add(2)

	copyDir := func(dst io.Writer, src io.Reader) {
		buf := make([]byte, 64*1024)
		for {
			n, rerr := src.Read(buf)
			if n > 0 {
				if onActivity != nil {
					onActivity()
				}
				written := 0
				for written < n {
					w, werr := dst.Write(buf[written:n])
					if w > 0 {
						written += w
					}
					if werr != nil {
						return
					}
				}
			}
			if rerr != nil {
				return
			}
		}
	}

	go func() {
		defer wg.Done()
		copyDir(c1, c2)
		if cw, ok := c1.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		copyDir(c2, c1)
		_ = c2.Close()
	}()
	wg.Wait()
}
