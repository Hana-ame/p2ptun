package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"io"
	"log"
	"math/big"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/quic-go/quic-go"
)

const quicDataChannelLabel = "p2ptun-quic"

// dcPacketConn 将 WebRTC DataChannel (Unordered, MaxRetransmits=0) 适配为 net.PacketConn,
// 供 quic-go 运行在其上。
type dcPacketConn struct {
	dc         *webrtc.DataChannel
	in         chan []byte
	localAddr  net.Addr
	remoteAddr net.Addr
	done       chan struct{}
	closeOnce  sync.Once

	readDeadline atomic.Value // time.Time
}

func newDCPacketConn(dc *webrtc.DataChannel, local, remote net.Addr) *dcPacketConn {
	c := &dcPacketConn{
		dc:         dc,
		in:         make(chan []byte, 4096),
		localAddr:  local,
		remoteAddr: remote,
		done:       make(chan struct{}),
	}
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		select {
		case <-c.done:
		case c.in <- msg.Data:
			globalStats.bytesRx.Add(uint64(len(msg.Data)))
		default:
			// 缓冲队列满时丢包（符合 UDP 语义，触发 QUIC 拥塞检测与单流重传）
		}
	})
	dc.OnClose(func() {
		_ = c.Close()
	})
	return c
}

func (c *dcPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	var timer *time.Timer
	var timeoutChan <-chan time.Time
	if dl, ok := c.readDeadline.Load().(time.Time); ok && !dl.IsZero() {
		dur := time.Until(dl)
		if dur <= 0 {
			return 0, nil, os.ErrDeadlineExceeded
		}
		timer = time.NewTimer(dur)
		defer timer.Stop()
		timeoutChan = timer.C
	}

	select {
	case <-c.done:
		return 0, nil, io.EOF
	case <-timeoutChan:
		return 0, nil, os.ErrDeadlineExceeded
	case data, ok := <-c.in:
		if !ok {
			return 0, nil, io.EOF
		}
		n := copy(p, data)
		return n, c.remoteAddr, nil
	}
}

func (c *dcPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	select {
	case <-c.done:
		return 0, io.EOF
	default:
	}
	if err := c.dc.Send(p); err != nil {
		return 0, err
	}
	globalStats.bytesTx.Add(uint64(len(p)))
	return len(p), nil
}

func (c *dcPacketConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.dc.Close()
	})
	return nil
}

func (c *dcPacketConn) LocalAddr() net.Addr { return c.localAddr }

func (c *dcPacketConn) SetDeadline(t time.Time) error {
	c.readDeadline.Store(t)
	return nil
}

func (c *dcPacketConn) SetReadDeadline(t time.Time) error {
	c.readDeadline.Store(t)
	return nil
}

func (c *dcPacketConn) SetWriteDeadline(t time.Time) error {
	return nil
}

func quicSecretALPN(secret string) string {
	h := sha256.Sum256([]byte("p2ptun-quic-alpn:" + secret))
	return "p2ptun-q-" + hex.EncodeToString(h[:8])
}

func generateQUICServerTLS(secret string) (*tls.Config, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "p2ptun"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert := tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  key,
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{quicSecretALPN(secret)},
	}, nil
}

func generateQUICClientTLS(secret string) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{quicSecretALPN(secret)},
	}
}

func defaultQUICConfig() *quic.Config {
	return &quic.Config{
		MaxIdleTimeout:                 45 * time.Second,
		KeepAlivePeriod:                10 * time.Second,
		DisablePathMTUDiscovery:        true,
		InitialPacketSize:              1200,
		MaxIncomingStreams:             1000,
		InitialStreamReceiveWindow:     1 << 20, // 1MB
		MaxStreamReceiveWindow:         4 << 20, // 4MB
		InitialConnectionReceiveWindow: 2 << 20, // 2MB
		MaxConnectionReceiveWindow:     16 << 20,
		EnableDatagrams:                true,
	}
}

// pipeStream pipes a TCP connection and a QUIC stream bidirectionally.
func pipeStream(tcpConn net.Conn, qStream quic.Stream) {
	tuneTCPConn(tcpConn)
	defer tcpConn.Close()
	defer qStream.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, _ = io.Copy(qStream, tcpConn)
		_ = qStream.Close() // 发送 QUIC STREAM FIN，通知对端本端已发送完毕
	}()

	go func() {
		defer wg.Done()
		_, _ = io.Copy(tcpConn, qStream)
		if cw, ok := tcpConn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()

	wg.Wait()
}

// runQUICExpose runs the QUIC server on the expose side over the given DataChannel.
func runQUICExpose(cfg config, sess *exposeSession, dc *webrtc.DataChannel) {
	defer sess.close()

	sAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 4430}
	cAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 4431}
	pConn := newDCPacketConn(dc, sAddr, cAddr)
	defer pConn.Close()

	tlsConf, err := generateQUICServerTLS(cfg.secret)
	if err != nil {
		log.Printf("expose: [%s] quic tls error: %v", sess.key, err)
		return
	}

	tr := &quic.Transport{Conn: pConn}
	defer tr.Close()

	listener, err := tr.Listen(tlsConf, defaultQUICConfig())
	if err != nil {
		log.Printf("expose: [%s] quic listen error: %v", sess.key, err)
		return
	}
	defer listener.Close()

	go func() {
		<-sess.ctx.Done()
		_ = listener.Close()
		_ = tr.Close()
	}()

	for {
		qConn, err := listener.Accept(sess.ctx)
		if err != nil {
			return
		}
		log.Printf("expose: [%s] quic connection established with client", sess.key)

		// RFC 9221 datagram echo loop for RTT measurement
		go func(conn quic.Connection) {
			for {
				msg, err := conn.ReceiveDatagram(sess.ctx)
				if err != nil {
					return
				}
				_ = conn.SendDatagram(msg)
			}
		}(qConn)

		go func(conn quic.Connection) {
			defer conn.CloseWithError(0, "session closed")
			for {
				stream, err := conn.AcceptStream(sess.ctx)
				if err != nil {
					return
				}
				globalStats.activeStreams.Add(1)
				go func(st quic.Stream) {
					defer globalStats.activeStreams.Add(-1)
					dialCtx, cancel := context.WithTimeout(sess.ctx, 5*time.Second)
					targetConn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", cfg.target)
					cancel()
					if err != nil {
						log.Printf("expose: dial %s failed: %v", cfg.target, err)
						_ = st.Close()
						return
					}
					pipeStream(targetConn, st)
				}(stream)
			}
		}(qConn)
	}
}

// runQUICConnect runs the QUIC client on the connect side over the given DataChannel.
func runQUICConnect(s *tunnelSess, dc *webrtc.DataChannel) {
	cAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 4431}
	sAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 4430}
	pConn := newDCPacketConn(dc, cAddr, sAddr)

	tlsConf := generateQUICClientTLS(s.cfg.secret)
	tr := &quic.Transport{Conn: pConn}

	go func() {
		<-s.ctx.Done()
		_ = tr.Close()
		_ = pConn.Close()
	}()

	dialCtx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
	qConn, err := tr.Dial(dialCtx, sAddr, tlsConf, defaultQUICConfig())
	cancel()
	if err != nil {
		log.Printf("connect: quic dial error: %v", err)
		s.cancelFn()
		return
	}
	log.Printf("connect: quic handshake success (alpn=%s)", quicSecretALPN(s.cfg.secret))

	s.quicMu.Lock()
	s.quicConn = qConn
	s.quicMu.Unlock()
	s.readyOnce.Do(func() { close(s.trReady) })

	// RFC 9221 datagram RTT probe loop
	go func() {
		interval := s.cfg.keepalive
		if interval <= 0 {
			interval = 5 * time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
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
			msg, err := qConn.ReceiveDatagram(s.ctx)
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

	<-qConn.Context().Done()
	log.Printf("connect: quic connection closed: %v", context.Cause(qConn.Context()))
	s.cancelFn()
}
