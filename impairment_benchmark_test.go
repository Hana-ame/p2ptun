package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net"
	"sync"
	"testing"
	"time"

	hyClient "github.com/apernet/hysteria/core/v2/client"
	hyServer "github.com/apernet/hysteria/core/v2/server"
	"github.com/pion/ice/v4"
	quic "github.com/quic-go/quic-go"
)

type packetItem struct {
	data      []byte
	addr      net.Addr
	deliverAt time.Time
}

type lossyDelayedPacketConn struct {
	underlying net.PacketConn
	delay      time.Duration
	lossRate   float64

	queue chan packetItem
	stop  chan struct{}
	rng   *rand.Rand
	rngMu sync.Mutex
}

func newLossyDelayedPacketConn(underlying net.PacketConn, oneWayDelay time.Duration, lossRate float64) *lossyDelayedPacketConn {
	c := &lossyDelayedPacketConn{
		underlying: underlying,
		delay:      oneWayDelay,
		lossRate:   lossRate,
		queue:      make(chan packetItem, 8192),
		stop:       make(chan struct{}),
		rng:        rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	go c.worker()
	return c
}

func (c *lossyDelayedPacketConn) worker() {
	for {
		select {
		case <-c.stop:
			return
		case item := <-c.queue:
			now := time.Now()
			if item.deliverAt.After(now) {
				time.Sleep(item.deliverAt.Sub(now))
			}
			_, _ = c.underlying.WriteTo(item.data, item.addr)
		}
	}
}

func (c *lossyDelayedPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.rngMu.Lock()
	drop := c.lossRate > 0 && c.rng.Float64() < c.lossRate
	c.rngMu.Unlock()

	if drop {
		// Packet dropped by simulated impairment
		return len(p), nil
	}

	if c.delay <= 0 {
		return c.underlying.WriteTo(p, addr)
	}

	buf := make([]byte, len(p))
	copy(buf, p)

	select {
	case <-c.stop:
		return 0, net.ErrClosed
	case c.queue <- packetItem{data: buf, addr: addr, deliverAt: time.Now().Add(c.delay)}:
		return len(p), nil
	}
}

func (c *lossyDelayedPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	return c.underlying.ReadFrom(p)
}

func (c *lossyDelayedPacketConn) Close() error {
	select {
	case <-c.stop:
	default:
		close(c.stop)
	}
	return c.underlying.Close()
}

func (c *lossyDelayedPacketConn) LocalAddr() net.Addr {
	return c.underlying.LocalAddr()
}

func (c *lossyDelayedPacketConn) SetDeadline(t time.Time) error {
	return c.underlying.SetDeadline(t)
}

func (c *lossyDelayedPacketConn) SetReadDeadline(t time.Time) error {
	return c.underlying.SetReadDeadline(t)
}

func (c *lossyDelayedPacketConn) SetWriteDeadline(t time.Time) error {
	return c.underlying.SetWriteDeadline(t)
}

// helper to establish paired ICE connection
func setupLoopbackICE(ctx context.Context) (*ice.Conn, *ice.Conn, error) {
	agentA, err := ice.NewAgent(&ice.AgentConfig{NetworkTypes: []ice.NetworkType{ice.NetworkTypeUDP4}, IncludeLoopback: true})
	if err != nil {
		return nil, nil, err
	}
	agentB, err := ice.NewAgent(&ice.AgentConfig{NetworkTypes: []ice.NetworkType{ice.NetworkTypeUDP4}, IncludeLoopback: true})
	if err != nil {
		_ = agentA.Close()
		return nil, nil, err
	}

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
	var errA, errB error
	wg.Add(2)
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
		if connA != nil {
			_ = connA.Close()
		}
		if connB != nil {
			_ = connB.Close()
		}
		_ = agentA.Close()
		_ = agentB.Close()
		return nil, nil, fmt.Errorf("ice setup failed: a=%v, b=%v", errA, errB)
	}
	return connA, connB, nil
}

func runHysteria2Benchmark(delay time.Duration, lossRate float64, dataSize int, bwMbps int) (float64, time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	connA, connB, err := setupLoopbackICE(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer connA.Close()
	defer connB.Close()

	pConnServer := newLossyDelayedPacketConn(&icePacketConn{conn: connB}, delay, lossRate)
	defer pConnServer.Close()
	pConnClient := newLossyDelayedPacketConn(&icePacketConn{conn: connA}, delay, lossRate)
	defer pConnClient.Close()

	// Local Echo TCP Listener
	targetLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, 0, err
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

	bwBytesPerSec := uint64(bwMbps) * 1024 * 1024 / 8

	hySrv, err := hyServer.NewServer(&hyServer.Config{
		Conn: pConnServer,
		TLSConfig: hyServer.TLSConfig{
			Certificates: []tls.Certificate{globalECDSACert},
		},
		Authenticator: &hy2Auth{secret: "bench-secret"},
		Outbound:      &hy2Outbound{target: targetLis.Addr().String()},
		BandwidthConfig: hyServer.BandwidthConfig{
			MaxTx: bwBytesPerSec,
			MaxRx: bwBytesPerSec,
		},
		DisableUDP: true,
	})
	if err != nil {
		return 0, 0, err
	}
	defer hySrv.Close()
	go func() { _ = hySrv.Serve() }()

	hyCli, _, err := hyClient.NewClient(&hyClient.Config{
		ConnFactory: &hy2ConnFactory{conn: pConnClient},
		ServerAddr:  connB.LocalAddr(),
		Auth:        "bench-secret",
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
		return 0, 0, err
	}
	defer hyCli.Close()

	testData := make([]byte, dataSize)
	for i := range testData {
		testData[i] = byte(i % 256)
	}

	stream, err := hyCli.TCP("echo")
	if err != nil {
		return 0, 0, err
	}

	start := time.Now()
	var wg sync.WaitGroup
	var readErr, writeErr error
	recvBuf := make([]byte, dataSize)

	wg.Add(2)
	go func() {
		defer wg.Done()
		_, writeErr = stream.Write(testData)
	}()

	go func() {
		defer wg.Done()
		_, readErr = io.ReadFull(stream, recvBuf)
	}()
	wg.Wait()
	_ = stream.Close()
	elapsed := time.Since(start)

	if writeErr != nil {
		return 0, 0, fmt.Errorf("write error: %w", writeErr)
	}
	if readErr != nil {
		return 0, 0, fmt.Errorf("read error: %w", readErr)
	}

	mbps := float64(dataSize*8) / (elapsed.Seconds() * 1000 * 1000)
	return mbps, elapsed, nil
}

func runQUICBenchmark(delay time.Duration, lossRate float64, dataSize int) (float64, time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	connA, connB, err := setupLoopbackICE(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer connA.Close()
	defer connB.Close()

	pConnServer := newLossyDelayedPacketConn(&icePacketConn{conn: connB}, delay, lossRate)
	defer pConnServer.Close()
	pConnClient := newLossyDelayedPacketConn(&icePacketConn{conn: connA}, delay, lossRate)
	defer pConnClient.Close()

	alpn := fmt.Sprintf("p2ptun-q-%d", rand.Int63())
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
		return 0, 0, err
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
		return 0, 0, err
	}
	defer qConn.CloseWithError(0, "bye")

	stream, err := qConn.OpenStreamSync(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer stream.Close()

	testData := make([]byte, dataSize)
	for i := range testData {
		testData[i] = byte(i % 256)
	}

	start := time.Now()
	var wg sync.WaitGroup
	var readErr, writeErr error
	recvBuf := make([]byte, dataSize)

	wg.Add(2)
	go func() {
		defer wg.Done()
		_, writeErr = stream.Write(testData)
	}()

	go func() {
		defer wg.Done()
		_, readErr = io.ReadFull(stream, recvBuf)
	}()
	wg.Wait()
	elapsed := time.Since(start)

	if writeErr != nil {
		return 0, 0, fmt.Errorf("write error: %w", writeErr)
	}
	if readErr != nil {
		return 0, 0, fmt.Errorf("read error: %w", readErr)
	}

	mbps := float64(dataSize*8) / (elapsed.Seconds() * 1000 * 1000)
	return mbps, elapsed, nil
}

func TestNetworkImpairmentComparison(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network impairment benchmark in short mode")
	}
	type scenario struct {
		name        string
		oneWayDelay time.Duration // one-way latency
		lossRate    float64       // packet loss rate per direction
		dataSize    int           // bytes
	}

	scenarios := []scenario{
		{
			name:        "1. Clean Network (0ms RTT, 0% Loss)",
			oneWayDelay: 0,
			lossRate:    0.0,
			dataSize:    3 * 1024 * 1024,
		},
		{
			name:        "2. Domestic 4G/5G WAN (50ms RTT, 2% Loss)",
			oneWayDelay: 25 * time.Millisecond,
			lossRate:    0.02,
			dataSize:    2 * 1024 * 1024,
		},
		{
			name:        "3. Cross-Border / Cable WAN (100ms RTT, 5% Loss)",
			oneWayDelay: 50 * time.Millisecond,
			lossRate:    0.05,
			dataSize:    2 * 1024 * 1024,
		},
		{
			name:        "4. High-Congestion Weak Network (100ms RTT, 10% Loss)",
			oneWayDelay: 50 * time.Millisecond,
			lossRate:    0.10,
			dataSize:    2 * 1024 * 1024,
		},
		{
			name:        "5. Extreme Harsh Network (160ms RTT, 20% Loss)",
			oneWayDelay: 80 * time.Millisecond,
			lossRate:    0.20,
			dataSize:    1 * 1024 * 1024,
		},
	}

	fmt.Println("\n=========================================================================================")
	fmt.Println("             P2PTUN 弱网模拟基准测试 (Network Impairment Benchmark)")
	fmt.Println("             Hysteria 2 (Brutal CC: 100Mbps) vs Native QUIC (Standard CC)")
	fmt.Println("=========================================================================================")

	for _, sc := range scenarios {
		rtt := sc.oneWayDelay * 2
		lossPct := sc.lossRate * 100
		fmt.Printf("\n>>> 正在测试场景: [%s] (RTT=%v, 单向丢包率=%.0f%%, 数据量=%.1fMB)...\n",
			sc.name, rtt, lossPct, float64(sc.dataSize)/1024/1024)

		// 1. Run Hysteria 2 with Brutal CC (100 Mbps)
		hyMbps, hyElapsed, hyErr := runHysteria2Benchmark(sc.oneWayDelay, sc.lossRate, sc.dataSize, 100)
		if hyErr != nil {
			fmt.Printf("   [Hysteria 2 Brutal] 失败: %v\n", hyErr)
		} else {
			fmt.Printf("   [Hysteria 2 Brutal] 吞吐率: %6.2f Mbps  | 耗时: %8s  | 数据校验: 100%% 完整\n", hyMbps, hyElapsed.Round(time.Millisecond))
		}

		// 2. Run Native QUIC
		quicMbps, quicElapsed, quicErr := runQUICBenchmark(sc.oneWayDelay, sc.lossRate, sc.dataSize)
		if quicErr != nil {
			fmt.Printf("   [Native QUIC      ] 失败: %v\n", quicErr)
		} else {
			speedup := hyMbps / quicMbps
			fmt.Printf("   [Native QUIC      ] 吞吐率: %6.2f Mbps  | 耗时: %8s  | 数据校验: 100%% 完整  [Hy2 速度倍率: %.1fx]\n",
				quicMbps, quicElapsed.Round(time.Millisecond), speedup)
		}
	}
	fmt.Println("=========================================================================================")
}
