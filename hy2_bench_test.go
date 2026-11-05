package main

import (
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hyClient "github.com/apernet/hysteria/core/v2/client"
	hyServer "github.com/apernet/hysteria/core/v2/server"
	"github.com/pion/webrtc/v4"
)

func runHy2LoopbackBenchmark(t *testing.T, numStreams int, dataPerStream int, bwMbps int) (float64, time.Duration) {
	t.Helper()

	// 1. TCP Echo server
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

	// 2. WebRTC DataChannel pair
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

	sAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 58830}
	cAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 58831}

	ePConn := newDCPacketConn(dcB, sAddr, cAddr)
	defer ePConn.Close()
	cPConn := newDCPacketConn(dcA, cAddr, sAddr)
	defer cPConn.Close()

	tlsConf, err := generateHy2ServerTLS()
	if err != nil {
		t.Fatal(err)
	}

	bwBytes := uint64(bwMbps) * 1024 * 1024 / 8

	// 3. Hysteria2 Server
	srv, err := hyServer.NewServer(&hyServer.Config{
		Conn:          ePConn,
		TLSConfig:     tlsConf,
		Authenticator: &mockAuth{secret: "bench-secret"},
		Outbound:      &mockOutbound{target: echoLis.Addr().String()},
		BandwidthConfig: hyServer.BandwidthConfig{
			MaxTx: bwBytes,
			MaxRx: bwBytes,
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

	time.Sleep(50 * time.Millisecond)

	// 4. Hysteria2 Client
	cli, _, err := hyClient.NewClient(&hyClient.Config{
		ConnFactory: &mockConnFactory{conn: cPConn},
		ServerAddr:  sAddr,
		Auth:        "bench-secret",
		TLSConfig: hyClient.TLSConfig{
			InsecureSkipVerify: true,
		},
		BandwidthConfig: hyClient.BandwidthConfig{
			MaxTx: bwBytes,
			MaxRx: bwBytes,
		},
		FastOpen: true,
	})
	if err != nil {
		t.Fatalf("hyClient.NewClient: %v", err)
	}
	defer cli.Close()

	// 5. Benchmark payload
	data := make([]byte, dataPerStream)
	_, _ = rand.Read(data)

	var totalRecv atomic.Int64
	start := time.Now()
	var wg sync.WaitGroup

	for i := 0; i < numStreams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stream, err := cli.TCP("echo")
			if err != nil {
				t.Errorf("cli.TCP: %v", err)
				return
			}
			defer stream.Close()

			var swg sync.WaitGroup
			swg.Add(2)
			go func() {
				defer swg.Done()
				_, _ = stream.Write(data)
				_ = stream.Close()
			}()
			go func() {
				defer swg.Done()
				buf := make([]byte, 32*1024)
				n, _ := io.CopyBuffer(io.Discard, stream, buf)
				totalRecv.Add(n)
			}()
			swg.Wait()
		}()
	}

	wg.Wait()
	elapsed := time.Since(start)

	totalTransferred := int64(numStreams)*int64(dataPerStream) + totalRecv.Load()
	mbps := float64(totalTransferred*8) / elapsed.Seconds() / 1e6
	return mbps, elapsed
}

func TestHysteria2LoopbackBenchmark(t *testing.T) {
	fmt.Println("\n=======================================================")
	fmt.Println("     Hysteria 2 (Brutal CC) Loopback Benchmark")
	fmt.Println("=======================================================")

	t.Run("Hy2_1x2MB_50Mbps", func(t *testing.T) {
		mbps, dur := runHy2LoopbackBenchmark(t, 1, 2*1024*1024, 50)
		fmt.Printf("  [Hy2 Brutal 50M] 1 x 2MB:   %6.1f Mbps  (耗时 %5.2fs)\n", mbps, dur.Seconds())
	})

	t.Run("Hy2_5x1MB_100Mbps", func(t *testing.T) {
		mbps, dur := runHy2LoopbackBenchmark(t, 5, 1024*1024, 100)
		fmt.Printf("  [Hy2 Brutal 100M] 5 x 1MB:  %6.1f Mbps  (耗时 %5.2fs)\n", mbps, dur.Seconds())
	})

	t.Run("Hy2_10x512KB_100Mbps", func(t *testing.T) {
		mbps, dur := runHy2LoopbackBenchmark(t, 10, 512*1024, 100)
		fmt.Printf("  [Hy2 Brutal 100M] 10 x 512K: %6.1f Mbps  (耗时 %5.2fs)\n", mbps, dur.Seconds())
	})
	fmt.Println("=======================================================")
}
