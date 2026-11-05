package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
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

// icePacketConn wraps *ice.Conn into net.PacketConn for QUIC and Hysteria 2
type icePacketConn struct {
	conn *ice.Conn
}

func (c *icePacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, err := c.conn.Read(p)
	if err != nil {
		return 0, nil, err
	}
	return n, c.conn.RemoteAddr(), nil
}

func (c *icePacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	return c.conn.Write(p)
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

// buildICEAgent creates an ice.Agent configured with STUN servers
func buildICEAgent(cfg config) (*ice.Agent, error) {
	uris := parseStunURIs(cfg.stun)
	agentConfig := &ice.AgentConfig{
		Urls:         uris,
		NetworkTypes: []ice.NetworkType{ice.NetworkTypeUDP4, ice.NetworkTypeUDP6},
	}
	return ice.NewAgent(agentConfig)
}

// runNativeUDPExpose handles the expose side: coordinated via SCTP DataChannel, connects native UDP.
func runNativeUDPExpose(cfg config, dc *webrtc.DataChannel, cancelFn func()) {
	defer cancelFn()

	log.Printf("expose: SCTP control channel open, coordinating native UDP...")

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
		data, _ := json.Marshal(msg)
		_ = dc.Send(data)
	}

	var remoteUfrag, remotePwd string
	var protoChoice string = cfg.proto
	if protoChoice == "" {
		protoChoice = "hy2"
	}
	var bwMbps int = cfg.bandwidthMbps
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

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		var sm udpScheduleMsg
		if err := json.Unmarshal(msg.Data, &sm); err != nil {
			return
		}
		switch sm.Type {
		case "init":
			remoteUfrag = sm.Ufrag
			remotePwd = sm.Pwd
			if sm.Proto != "" {
				protoChoice = sm.Proto
			}
			if sm.Bandwidth > 0 {
				bwMbps = sm.Bandwidth
			}
			// Reply with local creds
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
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	select {
	case <-ctx.Done():
		log.Printf("expose: timed out waiting for init on SCTP control channel")
		return
	case <-readyCh:
	}

	log.Printf("expose: punch native UDP (proto=%s, bandwidth=%d Mbps)...", protoChoice, bwMbps)
	iceConn, err := agent.Accept(ctx, remoteUfrag, remotePwd)
	if err != nil {
		log.Printf("expose: native UDP accept failed: %v", err)
		return
	}
	defer iceConn.Close()

	log.Printf("expose: native UDP punched! local=%s remote=%s", iceConn.LocalAddr(), iceConn.RemoteAddr())
	pConn := &icePacketConn{conn: iceConn}

	// Dispatch to chosen transport on bare UDP
	if protoChoice == "quic" {
		runNativeQUICExpose(cfg, pConn)
	} else {
		runNativeHy2Expose(cfg, pConn, bwMbps)
	}
}

// runNativeUDPConnect handles the connect side: coordinated via SCTP DataChannel, connects native UDP.
func runNativeUDPConnect(s *tunnelSess, dc *webrtc.DataChannel) {
	log.Printf("connect: SCTP control channel open, coordinating native UDP...")

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
		data, _ := json.Marshal(msg)
		_ = dc.Send(data)
	}

	protoChoice := s.cfg.proto
	if protoChoice == "" {
		protoChoice = "hy2"
	}
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

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		var sm udpScheduleMsg
		if err := json.Unmarshal(msg.Data, &sm); err != nil {
			return
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
	})

	// Send init with local creds & desired proto/bandwidth
	sendCtrl(udpScheduleMsg{
		Type:      "init",
		Proto:     protoChoice,
		Bandwidth: bwMbps,
		Ufrag:     ufragLocal,
		Pwd:       pwdLocal,
	})
	_ = agent.GatherCandidates()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	select {
	case <-ctx.Done():
		log.Printf("connect: timed out waiting for init from expose on SCTP control channel")
		s.cancelFn()
		return
	case <-readyCh:
	}

	log.Printf("connect: dialing native UDP to peer (proto=%s, bandwidth=%d Mbps)...", protoChoice, bwMbps)
	iceConn, err := agent.Dial(ctx, remoteUfrag, remotePwd)
	if err != nil {
		log.Printf("connect: native UDP dial failed: %v", err)
		s.cancelFn()
		return
	}
	defer iceConn.Close()

	log.Printf("connect: native UDP punched! local=%s remote=%s", iceConn.LocalAddr(), iceConn.RemoteAddr())
	pConn := &icePacketConn{conn: iceConn}

	if protoChoice == "quic" {
		runNativeQUICConnect(s, pConn)
	} else {
		runNativeHy2Connect(s, pConn, bwMbps)
	}
}

// runNativeHy2Expose runs Hysteria 2 server on top of bare UDP packet conn
func runNativeHy2Expose(cfg config, pConn net.PacketConn, bwMbps int) {
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
		Outbound:      &hy2Outbound{target: cfg.target},
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

	log.Printf("expose: native Hysteria 2 server active (Brutal CC %d Mbps)", bwMbps)
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
	log.Printf("connect: native Hysteria 2 handshake success! (Brutal CC %d Mbps, tx=%d bps)", bwMbps, info.Tx)

	s.hy2Mu.Lock()
	s.hy2Client = cli
	s.hy2Mu.Unlock()
	s.readyOnce.Do(func() { close(s.trReady) })

	<-s.cancel
	_ = cli.Close()
	s.cancelFn()
}

// runNativeQUICExpose runs quic-go server on bare UDP packet conn
func runNativeQUICExpose(cfg config, pConn net.PacketConn) {
	if globalECDSACertErr != nil {
		log.Printf("expose: ecdsa cert error: %v", globalECDSACertErr)
		return
	}

	alpn := "p2ptun-raw-quic"
	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{globalECDSACert},
		NextProtos:   []string{alpn},
	}
	quicConf := &quic.Config{
		KeepAlivePeriod: 5 * time.Second,
		MaxIdleTimeout:  30 * time.Second,
		EnableDatagrams: false,
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

	qConn, err := listener.Accept(context.Background())
	if err != nil {
		log.Printf("expose: quic accept: %v", err)
		return
	}
	defer qConn.CloseWithError(0, "bye")

	for {
		stream, err := qConn.AcceptStream(context.Background())
		if err != nil {
			return
		}
		go func(qs *quic.Stream) {
			defer qs.Close()
			dest, err := (&net.Dialer{Timeout: 5 * time.Second}).Dial("tcp", cfg.target)
			if err != nil {
				log.Printf("expose: dial target %s: %v", cfg.target, err)
				return
			}
			defer dest.Close()
			tuneTCPConn(dest)
			pipeBidirectional(dest, qs)
		}(stream)
	}
}

// runNativeQUICConnect runs quic-go client on bare UDP packet conn
func runNativeQUICConnect(s *tunnelSess, pConn net.PacketConn) {
	alpn := "p2ptun-raw-quic"
	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{alpn},
	}
	quicConf := &quic.Config{
		KeepAlivePeriod: 5 * time.Second,
		MaxIdleTimeout:  30 * time.Second,
	}

	tr := &quic.Transport{Conn: pConn}
	defer tr.Close()

	qConn, err := tr.Dial(context.Background(), pConn.LocalAddr(), tlsConf, quicConf)
	if err != nil {
		log.Printf("connect: native QUIC dial: %v", err)
		s.cancelFn()
		return
	}
	defer qConn.CloseWithError(0, "bye")

	log.Printf("connect: native QUIC handshake success! (ALPN=%s)", alpn)

	s.quicMu.Lock()
	s.quicConn = qConn
	s.quicMu.Unlock()
	s.readyOnce.Do(func() { close(s.trReady) })

	<-s.cancel
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
}

func (o *hy2Outbound) TCP(reqAddr string) (net.Conn, error) {
	c, err := (&net.Dialer{Timeout: 5 * time.Second}).Dial("tcp", o.target)
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

func pipeBidirectional(c1 net.Conn, c2 io.ReadWriteCloser) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		buf := make([]byte, 64*1024)
		_, _ = io.CopyBuffer(c1, c2, buf)
		if tc, ok := c1.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, 64*1024)
		_, _ = io.CopyBuffer(c2, c1, buf)
		_ = c2.Close()
	}()
	wg.Wait()
}

func tuneTCPConn(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
		_ = tc.SetReadBuffer(1024 * 1024)
		_ = tc.SetWriteBuffer(1024 * 1024)
	}
}
