package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	hyClient "github.com/apernet/hysteria/core/v2/client"
	hyServer "github.com/apernet/hysteria/core/v2/server"
	"github.com/pion/webrtc/v4"
)

type mockConnFactory struct {
	conn net.PacketConn
}

func (f *mockConnFactory) New(net.Addr) (net.PacketConn, error) {
	return f.conn, nil
}

type mockAuth struct {
	secret string
}

func (a *mockAuth) Authenticate(addr net.Addr, auth string, tx uint64) (bool, string) {
	if auth == a.secret {
		return true, "mock-user"
	}
	return false, ""
}

type mockOutbound struct {
	target string
}

func (o *mockOutbound) TCP(reqAddr string) (net.Conn, error) {
	return net.DialTimeout("tcp", o.target, 5*time.Second)
}

func (o *mockOutbound) UDP(reqAddr string) (hyServer.UDPConn, error) {
	return nil, io.EOF
}

func (o *mockOutbound) CheckUDP(reqAddr string) error {
	return io.EOF
}

func generateHy2ServerTLS() (hyServer.TLSConfig, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return hyServer.TLSConfig{}, err
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "p2ptun-hy2"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return hyServer.TLSConfig{}, err
	}
	return hyServer.TLSConfig{
		Certificates: []tls.Certificate{
			{Certificate: [][]byte{certDER}, PrivateKey: key},
		},
	}, nil
}

func TestHysteria2OverDataChannel(t *testing.T) {
	// TCP echo server
	echoLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLis.Close()

	go func() {
		for {
			c, err := echoLis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()

	// Create WebRTC DC pair
	api := webrtc.NewAPI()
	pcA, _ := api.NewPeerConnection(webrtc.Configuration{})
	pcB, _ := api.NewPeerConnection(webrtc.Configuration{})
	defer pcA.Close()
	defer pcB.Close()

	o := false
	mr := uint16(0)
	dcA, _ := pcA.CreateDataChannel("p2ptun-hy2", &webrtc.DataChannelInit{Ordered: &o, MaxRetransmits: &mr})

	var dcB *webrtc.DataChannel
	bReady := make(chan struct{})
	pcB.OnDataChannel(func(dc *webrtc.DataChannel) {
		dcB = dc
		dc.OnOpen(func() { close(bReady) })
	})
	aReady := make(chan struct{})
	dcA.OnOpen(func() { close(aReady) })

	pcA.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			_ = pcB.AddICECandidate(c.ToJSON())
		}
	})
	pcB.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			_ = pcA.AddICECandidate(c.ToJSON())
		}
	})

	offer, _ := pcA.CreateOffer(nil)
	_ = pcA.SetLocalDescription(offer)
	_ = pcB.SetRemoteDescription(offer)
	ans, _ := pcB.CreateAnswer(nil)
	_ = pcB.SetLocalDescription(ans)
	_ = pcA.SetRemoteDescription(ans)

	select {
	case <-aReady:
	case <-time.After(5 * time.Second):
		t.Fatal("dcA timeout")
	}
	select {
	case <-bReady:
	case <-time.After(5 * time.Second):
		t.Fatal("dcB timeout")
	}

	sAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 54430}
	cAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 54431}

	ePConn := newDCPacketConn(dcB, sAddr, cAddr)
	defer ePConn.Close()
	cPConn := newDCPacketConn(dcA, cAddr, sAddr)
	defer cPConn.Close()

	tlsConf, err := generateHy2ServerTLS()
	if err != nil {
		t.Fatal(err)
	}

	// Server
	srv, err := hyServer.NewServer(&hyServer.Config{
		Conn:          ePConn,
		TLSConfig:     tlsConf,
		Authenticator: &mockAuth{secret: "hy2secret"},
		Outbound:      &mockOutbound{target: echoLis.Addr().String()},
		BandwidthConfig: hyServer.BandwidthConfig{
			MaxTx: 50 * 1024 * 1024,
			MaxRx: 50 * 1024 * 1024,
		},
		DisableUDP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	go func() {
		_ = srv.Serve()
	}()

	time.Sleep(100 * time.Millisecond)

	// Client
	cli, info, err := hyClient.NewClient(&hyClient.Config{
		ConnFactory: &mockConnFactory{conn: cPConn},
		ServerAddr:  sAddr,
		Auth:        "hy2secret",
		TLSConfig: hyClient.TLSConfig{
			InsecureSkipVerify: true,
		},
		BandwidthConfig: hyClient.BandwidthConfig{
			MaxTx: 50 * 1024 * 1024,
			MaxRx: 50 * 1024 * 1024,
		},
		FastOpen: true,
	})
	if err != nil {
		t.Fatalf("hyClient.NewClient: %v", err)
	}
	defer cli.Close()

	t.Logf("Hysteria2 connected! Tx bandwidth = %d bps", info.Tx)

	// Open 5 concurrent TCP streams through Hysteria2
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			stream, err := cli.TCP("echo")
			if err != nil {
				t.Errorf("cli.TCP: %v", err)
				return
			}
			defer stream.Close()

			msg := []byte("hello hysteria2 stream")
			_, _ = stream.Write(msg)
			buf := make([]byte, len(msg))
			_, _ = io.ReadFull(stream, buf)
			if string(buf) != string(msg) {
				t.Errorf("mismatch: got %s, want %s", string(buf), string(msg))
			}
		}(i)
	}
	wg.Wait()
}
