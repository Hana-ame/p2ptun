package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"log"
	"math/big"
	"net"
	"time"

	hyClient "github.com/apernet/hysteria/core/v2/client"
	hyServer "github.com/apernet/hysteria/core/v2/server"
	"github.com/pion/webrtc/v4"
)

const hy2DataChannelLabel = "p2ptun-hy2"

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

func generateHy2TLS() (hyServer.TLSConfig, error) {
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

// runHy2Expose runs the Hysteria2 server over the DataChannel on the expose side.
func runHy2Expose(cfg config, sess *exposeSession, dc *webrtc.DataChannel) {
	defer sess.close()

	sAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 54430}
	cAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 54431}
	pConn := newDCPacketConn(dc, sAddr, cAddr)
	defer pConn.Close()

	tlsConf, err := generateHy2TLS()
	if err != nil {
		log.Printf("expose: [%s] hy2 tls error: %v", sess.key, err)
		return
	}

	bwMbps := cfg.bandwidthMbps
	if bwMbps <= 0 {
		bwMbps = 50
	}
	bwBytesPerSec := uint64(bwMbps) * 1024 * 1024 / 8

	srv, err := hyServer.NewServer(&hyServer.Config{
		Conn:          pConn,
		TLSConfig:     tlsConf,
		Authenticator: &hy2Auth{secret: cfg.secret},
		Outbound:      &hy2Outbound{target: cfg.target},
		BandwidthConfig: hyServer.BandwidthConfig{
			MaxTx: bwBytesPerSec,
			MaxRx: bwBytesPerSec,
		},
		DisableUDP: true,
	})
	if err != nil {
		log.Printf("expose: [%s] hy2 server error: %v", sess.key, err)
		return
	}
	defer srv.Close()

	go func() {
		<-sess.ctx.Done()
		_ = srv.Close()
	}()

	log.Printf("expose: [%s] hysteria2 server running (brutal bandwidth=%d Mbps)", sess.key, bwMbps)
	_ = srv.Serve()
}

// runHy2Connect runs the Hysteria2 client over the DataChannel on the connect side.
func runHy2Connect(s *tunnelSess, dc *webrtc.DataChannel) {
	cAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 54431}
	sAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 54430}
	pConn := newDCPacketConn(dc, cAddr, sAddr)

	bwMbps := s.cfg.bandwidthMbps
	if bwMbps <= 0 {
		bwMbps = 50
	}
	bwBytesPerSec := uint64(bwMbps) * 1024 * 1024 / 8

	cli, info, err := hyClient.NewClient(&hyClient.Config{
		ConnFactory: &hy2ConnFactory{conn: pConn},
		ServerAddr:  sAddr,
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
		log.Printf("connect: hysteria2 dial error: %v", err)
		_ = pConn.Close()
		s.cancelFn()
		return
	}
	log.Printf("connect: hysteria2 handshake success (Brutal CC bandwidth=%d Mbps, tx=%d bps)", bwMbps, info.Tx)

	s.hy2Mu.Lock()
	s.hy2Client = cli
	s.hy2Mu.Unlock()
	s.readyOnce.Do(func() { close(s.trReady) })

	<-s.ctx.Done()
	_ = cli.Close()
	_ = pConn.Close()
	s.cancelFn()
}
