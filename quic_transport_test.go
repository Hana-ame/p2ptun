package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

func TestQUICOverDataChannelDirect(t *testing.T) {
	echoLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	defer echoLis.Close()

	go func() {
		for {
			conn, err := echoLis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	m := webrtc.MediaEngine{}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(&m))

	pcServer, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("pcServer: %v", err)
	}
	defer pcServer.Close()

	pcClient, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("pcClient: %v", err)
	}
	defer pcClient.Close()

	pcServer.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			_ = pcClient.AddICECandidate(c.ToJSON())
		}
	})
	pcClient.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			_ = pcServer.AddICECandidate(c.ToJSON())
		}
	})

	serverDone := make(chan struct{})
	clientDone := make(chan struct{})
	secret := "test-secret-1234"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess := &exposeSession{
		key:    "test-sess",
		cfg:    config{secret: secret, target: echoLis.Addr().String()},
		reg:    newStreamReg(),
		ctx:    ctx,
		cancel: cancel,
		done:   serverDone,
	}

	pcServer.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnOpen(func() {
			if dc.Label() == quicDataChannelLabel {
				go runQUICExpose(sess.cfg, sess, dc)
			}
		})
	})

	ordered := false
	maxRetr := uint16(0)
	dcClient, err := pcClient.CreateDataChannel(quicDataChannelLabel, &webrtc.DataChannelInit{
		Ordered:        &ordered,
		MaxRetransmits: &maxRetr,
	})
	if err != nil {
		t.Fatalf("create dcClient: %v", err)
	}

	tunSess := &tunnelSess{
		cfg:       config{secret: secret, keepalive: 1 * time.Second},
		trReady:   make(chan struct{}),
		ctx:       ctx,
		ctxCancel: cancel,
		cancel:    clientDone,
	}

	dcClient.OnOpen(func() {
		go runQUICConnect(tunSess, dcClient)
	})

	offer, err := pcClient.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	_ = pcClient.SetLocalDescription(offer)
	_ = pcServer.SetRemoteDescription(offer)

	answer, err := pcServer.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("create answer: %v", err)
	}
	_ = pcServer.SetLocalDescription(answer)
	_ = pcClient.SetRemoteDescription(answer)

	select {
	case <-tunSess.trReady:
	case <-time.After(10 * time.Second):
		t.Fatalf("quic tunnel not ready after 10s")
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(streamIdx int) {
			defer wg.Done()
			tunSess.quicMu.Lock()
			qConn := tunSess.quicConn
			tunSess.quicMu.Unlock()
			if qConn == nil {
				t.Errorf("qConn is nil")
				return
			}
			st, err := qConn.OpenStreamSync(ctx)
			if err != nil {
				t.Errorf("open stream: %v", err)
				return
			}
			defer st.Close()

			msg := []byte(fmt.Sprintf("quic-packet-stream-payload-test-%d", streamIdx))
			if _, err := st.Write(msg); err != nil {
				t.Errorf("stream write: %v", err)
				return
			}
			_ = st.Close()

			buf := make([]byte, len(msg))
			if _, err := io.ReadFull(st, buf); err != nil {
				t.Errorf("stream read: %v", err)
				return
			}
			if !bytes.Equal(buf, msg) {
				t.Errorf("mismatch: got %s, want %s", string(buf), string(msg))
			}
		}(i)
	}
	wg.Wait()
}
