package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	quic "github.com/quic-go/quic-go"
)

const quicDataChannelLabel = "p2ptun-quic"

// dcPacketConn 将 WebRTC DataChannel (Unordered, MaxRetransmits=0) 适配为 net.PacketConn,
// 供兼容模式 (-proto dc-quic) 及单测运行 quic-go 使用。
type dcPacketConn struct {
	dc         *webrtc.DataChannel
	in         chan []byte
	lowCh      chan struct{}
	localAddr  net.Addr
	remoteAddr net.Addr
	done       chan struct{}
	closeOnce  sync.Once

	deadlineMu  sync.Mutex
	readDl      time.Time
	deadlineSig chan struct{}
}

func newDCPacketConn(dc *webrtc.DataChannel, local, remote net.Addr) *dcPacketConn {
	c := &dcPacketConn{
		dc:          dc,
		in:          make(chan []byte, 4096),
		lowCh:       make(chan struct{}, 1),
		localAddr:   local,
		remoteAddr:  remote,
		done:        make(chan struct{}),
		deadlineSig: make(chan struct{}, 1),
	}
	dc.SetBufferedAmountLowThreshold(128 << 10)
	dc.OnBufferedAmountLow(func() {
		select {
		case c.lowCh <- struct{}{}:
		default:
		}
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		cp := make([]byte, len(msg.Data))
		copy(cp, msg.Data)
		select {
		case <-c.done:
		case c.in <- cp:
			globalStats.bytesRx.Add(uint64(len(cp)))
		default:
		}
	})
	dc.OnClose(func() {
		_ = c.Close()
	})
	dc.OnError(func(err error) {
		_ = c.Close()
	})
	return c
}

func (c *dcPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		c.deadlineMu.Lock()
		dl := c.readDl
		c.deadlineMu.Unlock()

		var timer *time.Timer
		var timeoutChan <-chan time.Time
		if !dl.IsZero() {
			dur := time.Until(dl)
			if dur <= 0 {
				return 0, nil, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(dur)
			timeoutChan = timer.C
		}

		select {
		case <-c.done:
			if timer != nil {
				timer.Stop()
			}
			return 0, nil, io.EOF
		case <-timeoutChan:
			return 0, nil, os.ErrDeadlineExceeded
		case <-c.deadlineSig:
			if timer != nil {
				timer.Stop()
			}
			continue
		case data, ok := <-c.in:
			if timer != nil {
				timer.Stop()
			}
			if !ok {
				return 0, nil, io.EOF
			}
			n := copy(p, data)
			return n, c.remoteAddr, nil
		}
	}
}

func (c *dcPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	for c.dc.BufferedAmount() > (512 << 10) {
		select {
		case <-c.done:
			return 0, io.EOF
		case <-c.lowCh:
		case <-time.After(15 * time.Millisecond):
			break
		}
		break
	}
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
	return c.SetReadDeadline(t)
}

func (c *dcPacketConn) SetReadDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	c.readDl = t
	c.deadlineMu.Unlock()
	select {
	case c.deadlineSig <- struct{}{}:
	default:
	}
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
	if globalECDSACertErr != nil {
		return nil, globalECDSACertErr
	}
	return &tls.Config{
		Certificates: []tls.Certificate{globalECDSACert},
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
		InitialStreamReceiveWindow:     4 << 20,  // 4MB
		MaxStreamReceiveWindow:         16 << 20, // 16MB
		InitialConnectionReceiveWindow: 8 << 20,  // 8MB
		MaxConnectionReceiveWindow:     32 << 20, // 32MB
		EnableDatagrams:                true,
	}
}

// pipeStream pipes a TCP connection and a QUIC stream bidirectionally (compatible with quic-go v0.59.0 *quic.Stream).
func pipeStream(tcpConn net.Conn, qStream *quic.Stream) {
	tuneTCPConn(tcpConn)
	defer tcpConn.Close()
	defer qStream.Close()
	pipeBidirectional(tcpConn, qStream, nil)
}

// runQUICExpose runs the QUIC server on the expose side over the given DataChannel (-proto dc-quic compatibility).
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

		go func(conn *quic.Conn) {
			for {
				msg, err := conn.ReceiveDatagram(sess.ctx)
				if err != nil {
					return
				}
				_ = conn.SendDatagram(msg)
			}
		}(qConn)

		go func(conn *quic.Conn) {
			defer conn.CloseWithError(0, "session closed")
			for {
				stream, err := conn.AcceptStream(sess.ctx)
				if err != nil {
					return
				}
				globalStats.activeStreams.Add(1)
				go func(st *quic.Stream) {
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

// runQUICConnect runs the QUIC client on the connect side over the given DataChannel (-proto dc-quic compatibility).
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
	s.wasReady.Store(true)
	s.readyOnce.Do(func() { close(s.trReady) })

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
