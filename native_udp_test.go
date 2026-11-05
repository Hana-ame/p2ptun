package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	hyClient "github.com/apernet/hysteria/core/v2/client"
	hyServer "github.com/apernet/hysteria/core/v2/server"
	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	quic "github.com/quic-go/quic-go"
)

type mockOutboundEcho struct{}

func (m *mockOutboundEcho) TCP(reqAddr string) (net.Conn, error) {
	c1, c2 := net.Pipe()
	go func() {
		defer c2.Close()
		_, _ = io.Copy(c2, c2)
	}()
	return c1, nil
}

func (m *mockOutboundEcho) UDP(reqAddr string) (hyServer.UDPConn, error) {
	return nil, errors.New("udp unsupported")
}

func (m *mockOutboundEcho) CheckUDP(reqAddr string) error {
	return errors.New("udp unsupported")
}

func TestNativeICEAgentLoopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	agentA, err := ice.NewAgent(&ice.AgentConfig{NetworkTypes: []ice.NetworkType{ice.NetworkTypeUDP4}})
	if err != nil {
		t.Fatalf("agentA: %v", err)
	}
	defer agentA.Close()

	agentB, err := ice.NewAgent(&ice.AgentConfig{NetworkTypes: []ice.NetworkType{ice.NetworkTypeUDP4}})
	if err != nil {
		t.Fatalf("agentB: %v", err)
	}
	defer agentB.Close()

	ufragA, pwdA, _ := agentA.GetLocalUserCredentials()
	ufragB, pwdB, _ := agentB.GetLocalUserCredentials()

	var wg sync.WaitGroup
	wg.Add(2)

	_ = agentA.OnCandidate(func(c ice.Candidate) {
		if c != nil {
			parsed, _ := ice.UnmarshalCandidate(c.Marshal())
			_ = agentB.AddRemoteCandidate(parsed)
		}
	})

	_ = agentB.OnCandidate(func(c ice.Candidate) {
		if c != nil {
			parsed, _ := ice.UnmarshalCandidate(c.Marshal())
			_ = agentA.AddRemoteCandidate(parsed)
		}
	})

	var connA, connB *ice.Conn
	var errA, errB error

	go func() {
		defer wg.Done()
		connA, errA = agentA.Dial(ctx, ufragB, pwdB)
	}()

	go func() {
		defer wg.Done()
		connB, errB = agentB.Accept(ctx, ufragA, pwdA)
	}()

	_ = agentA.GatherCandidates()
	_ = agentB.GatherCandidates()

	wg.Wait()

	if errA != nil || errB != nil {
		t.Fatalf("ICE connect error: errA=%v, errB=%v", errA, errB)
	}
	defer connA.Close()
	defer connB.Close()

	msg := []byte("hello from A to B via raw native UDP!")
	if _, err := connA.Write(msg); err != nil {
		t.Fatalf("write A: %v", err)
	}

	buf := make([]byte, 1024)
	n, err := connB.Read(buf)
	if err != nil {
		t.Fatalf("read B: %v", err)
	}
	if string(buf[:n]) != string(msg) {
		t.Fatalf("mismatch: got %s, want %s", string(buf[:n]), string(msg))
	}
}

func TestHysteria2OverNativeICE(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	agentA, _ := ice.NewAgent(&ice.AgentConfig{NetworkTypes: []ice.NetworkType{ice.NetworkTypeUDP4}})
	defer agentA.Close()
	agentB, _ := ice.NewAgent(&ice.AgentConfig{NetworkTypes: []ice.NetworkType{ice.NetworkTypeUDP4}})
	defer agentB.Close()

	ufragA, pwdA, _ := agentA.GetLocalUserCredentials()
	ufragB, pwdB, _ := agentB.GetLocalUserCredentials()

	_ = agentA.OnCandidate(func(c ice.Candidate) {
		if c != nil {
			cand, _ := ice.UnmarshalCandidate(c.Marshal())
			_ = agentB.AddRemoteCandidate(cand)
		}
	})
	_ = agentB.OnCandidate(func(c ice.Candidate) {
		if c != nil {
			cand, _ := ice.UnmarshalCandidate(c.Marshal())
			_ = agentA.AddRemoteCandidate(cand)
		}
	})

	var connA, connB *ice.Conn
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		connA, _ = agentA.Dial(ctx, ufragB, pwdB)
	}()
	go func() {
		defer wg.Done()
		connB, _ = agentB.Accept(ctx, ufragA, pwdA)
	}()
	_ = agentA.GatherCandidates()
	_ = agentB.GatherCandidates()
	wg.Wait()

	defer connA.Close()
	defer connB.Close()

	pConnServer := &icePacketConn{conn: connB}
	pConnClient := &icePacketConn{conn: connA}

	hySrv, err := hyServer.NewServer(&hyServer.Config{
		Conn: pConnServer,
		TLSConfig: hyServer.TLSConfig{
			Certificates: []tls.Certificate{globalECDSACert},
		},
		Authenticator: &hy2Auth{secret: "test-secret"},
		Outbound:      &mockOutboundEcho{},
		BandwidthConfig: hyServer.BandwidthConfig{
			MaxTx: 50 * 1024 * 1024 / 8,
			MaxRx: 50 * 1024 * 1024 / 8,
		},
		DisableUDP: true,
	})
	if err != nil {
		t.Fatalf("hyServer: %v", err)
	}
	defer hySrv.Close()
	go func() { _ = hySrv.Serve() }()

	hyCli, info, err := hyClient.NewClient(&hyClient.Config{
		ConnFactory: &hy2ConnFactory{conn: pConnClient},
		ServerAddr:  connB.LocalAddr(),
		Auth:        "test-secret",
		TLSConfig: hyClient.TLSConfig{
			InsecureSkipVerify: true,
		},
		BandwidthConfig: hyClient.BandwidthConfig{
			MaxTx: 50 * 1024 * 1024 / 8,
			MaxRx: 50 * 1024 * 1024 / 8,
		},
		FastOpen: true,
	})
	if err != nil {
		t.Fatalf("hyClient: %v", err)
	}
	defer hyCli.Close()
	t.Logf("Hysteria2 over Native ICE connected! Brutal Tx=%d bps", info.Tx)

	stream, err := hyCli.TCP("echo")
	if err != nil {
		t.Fatalf("hyCli.TCP: %v", err)
	}
	defer stream.Close()

	payload := []byte("Testing Hysteria 2 over pure native ICE UDP connection!")
	if _, err := stream.Write(payload); err != nil {
		t.Fatalf("stream.Write: %v", err)
	}
	recv := make([]byte, len(payload))
	if _, err := io.ReadFull(stream, recv); err != nil {
		t.Fatalf("stream.ReadFull: %v", err)
	}
	if string(recv) != string(payload) {
		t.Fatalf("got %s, want %s", string(recv), string(payload))
	}
}

func TestQUICOverNativeICE(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	agentA, _ := ice.NewAgent(&ice.AgentConfig{NetworkTypes: []ice.NetworkType{ice.NetworkTypeUDP4}})
	defer agentA.Close()
	agentB, _ := ice.NewAgent(&ice.AgentConfig{NetworkTypes: []ice.NetworkType{ice.NetworkTypeUDP4}})
	defer agentB.Close()

	ufragA, pwdA, _ := agentA.GetLocalUserCredentials()
	ufragB, pwdB, _ := agentB.GetLocalUserCredentials()

	_ = agentA.OnCandidate(func(c ice.Candidate) {
		if c != nil {
			cand, _ := ice.UnmarshalCandidate(c.Marshal())
			_ = agentB.AddRemoteCandidate(cand)
		}
	})
	_ = agentB.OnCandidate(func(c ice.Candidate) {
		if c != nil {
			cand, _ := ice.UnmarshalCandidate(c.Marshal())
			_ = agentA.AddRemoteCandidate(cand)
		}
	})

	var connA, connB *ice.Conn
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		connA, _ = agentA.Dial(ctx, ufragB, pwdB)
	}()
	go func() {
		defer wg.Done()
		connB, _ = agentB.Accept(ctx, ufragA, pwdA)
	}()
	_ = agentA.GatherCandidates()
	_ = agentB.GatherCandidates()
	wg.Wait()

	defer connA.Close()
	defer connB.Close()

	pConnServer := &icePacketConn{conn: connB}
	pConnClient := &icePacketConn{conn: connA}

	alpn := "p2ptun-native-quic"
	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{globalECDSACert},
		NextProtos:   []string{alpn},
	}
	quicConf := &quic.Config{
		KeepAlivePeriod: 5 * time.Second,
		MaxIdleTimeout:  30 * time.Second,
	}

	trServer := &quic.Transport{Conn: pConnServer}
	defer trServer.Close()
	listener, err := trServer.Listen(serverTLS, quicConf)
	if err != nil {
		t.Fatalf("trServer.Listen: %v", err)
	}
	defer listener.Close()

	go func() {
		qConn, err := listener.Accept(ctx)
		if err != nil {
			return
		}
		defer qConn.CloseWithError(0, "bye")
		stream, err := qConn.AcceptStream(ctx)
		if err != nil {
			return
		}
		defer stream.Close()
		_, _ = io.Copy(stream, stream)
	}()

	trClient := &quic.Transport{Conn: pConnClient}
	defer trClient.Close()
	clientTLS := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{alpn},
	}
	qConn, err := trClient.Dial(ctx, connB.LocalAddr(), clientTLS, quicConf)
	if err != nil {
		t.Fatalf("trClient.Dial: %v", err)
	}
	defer qConn.CloseWithError(0, "bye")

	stream, err := qConn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("OpenStreamSync: %v", err)
	}
	defer stream.Close()

	payload := []byte("Hello Native QUIC over pure ICE UDP!")
	if _, err := stream.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	recv := make([]byte, len(payload))
	if _, err := io.ReadFull(stream, recv); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(recv) != string(payload) {
		t.Fatalf("got %s, want %s", string(recv), string(payload))
	}
}

// TestControlDataChannelSchedulingE2E tests full coordination over WebRTC DataChannel
// scheduling a native UDP Hysteria 2 connection and streaming TCP payload through a mock echo target.
func TestControlDataChannelSchedulingE2E(t *testing.T) {
	// 1. Mock TCP Target (Echo Server)
	targetLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen target: %v", err)
	}
	defer targetLis.Close()

	go func() {
		for {
			c, err := targetLis.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()

	// 2. Set up WebRTC PeerConnection Pair (simulating PeerJS signaling)
	pcExpose, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("pcExpose: %v", err)
	}
	defer pcExpose.Close()

	pcConnect, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("pcConnect: %v", err)
	}
	defer pcConnect.Close()

	pcExpose.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			_ = pcConnect.AddICECandidate(c.ToJSON())
		}
	})
	pcConnect.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			_ = pcExpose.AddICECandidate(c.ToJSON())
		}
	})

	exposeCfg := config{
		proto:         "hy2",
		bandwidthMbps: 50,
		secret:        "supersecret",
		target:        targetLis.Addr().String(),
	}
	connectCfg := config{
		proto:         "hy2",
		bandwidthMbps: 50,
		secret:        "supersecret",
	}

	cancelExpose := make(chan struct{})
	var onceExpose sync.Once
	cancelFnExpose := func() { onceExpose.Do(func() { close(cancelExpose) }) }

	connCh := make(chan net.Conn, 16)
	sessConnect := newTunnelSess(connectCfg, connCh, "remote-expose")
	defer sessConnect.teardown()

	pcExpose.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() == controlDataChannelLabel {
			go runNativeUDPExpose(exposeCfg, dc, cancelFnExpose)
		}
	})

	dcConnect, err := pcConnect.CreateDataChannel(controlDataChannelLabel, &webrtc.DataChannelInit{Ordered: boolPtr(true)})
	if err != nil {
		t.Fatalf("CreateDataChannel: %v", err)
	}
	dcConnect.OnOpen(func() {
		go runNativeUDPConnect(sessConnect, dcConnect)
	})

	// WebRTC SDP Handshake
	offer, err := pcConnect.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	_ = pcConnect.SetLocalDescription(offer)
	_ = pcExpose.SetRemoteDescription(offer)

	answer, err := pcExpose.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("CreateAnswer: %v", err)
	}
	_ = pcExpose.SetLocalDescription(answer)
	_ = pcConnect.SetRemoteDescription(answer)

	// Wait for native UDP + Hysteria 2 handshake
	select {
	case <-sessConnect.trReady:
		t.Log("End-to-End: Native UDP + Hysteria 2 handshake ready!")
	case <-time.After(10 * time.Second):
		t.Fatal("End-to-End: timeout waiting for Native UDP handshake")
	}

	// 3. Test multi-stream TCP transmission over native UDP
	for i := 0; i < 5; i++ {
		clientEnd, clientMock := net.Pipe()
		sessConnect.registerConn(clientEnd)

		payload := []byte(fmt.Sprintf("e2e-test-stream-packet-%d", i))
		if _, err := clientMock.Write(payload); err != nil {
			t.Fatalf("write stream %d: %v", i, err)
		}

		recv := make([]byte, len(payload))
		if _, err := io.ReadFull(clientMock, recv); err != nil {
			t.Fatalf("read stream %d: %v", i, err)
		}
		if string(recv) != string(payload) {
			t.Fatalf("stream %d mismatch: got %s, want %s", i, string(recv), string(payload))
		}
		clientMock.Close()
	}
	t.Log("End-to-End: 5 parallel streams successfully streamed and echoed over native UDP!")
}
