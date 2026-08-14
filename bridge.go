package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// DataChannel frame types (multiplex multiple TCP streams over one DC)
const (
	frameSyn  = 1
	frameData = 2
	frameFin  = 3

	dcHighWater   = 4 << 20 // pause sending above 4MB buffered
	dcLowWater    = 1 << 20 // resume after drops below 1MB
	dcSendTimeout = 60 * time.Second
)

type frameHandler func(typ uint8, id uint32, payload []byte)

// transport wraps a WebRTC DataChannel with frame framing + flow control.
type transport struct {
	dc    *webrtc.DataChannel
	lowCh chan struct{}

	onFrame frameHandler
	onClose func()

	mu     sync.Mutex
	closed bool
}

func newTransport(dc *webrtc.DataChannel, onFrame frameHandler) *transport {
	t := &transport{dc: dc, lowCh: make(chan struct{}, 1), onFrame: onFrame}
	dc.SetBufferedAmountLowThreshold(dcLowWater)
	dc.OnBufferedAmountLow(func() {
		select {
		case t.lowCh <- struct{}{}:
		default:
		}
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		t.dispatch(msg.Data)
	})
	return t
}

func (t *transport) setOnClose(fn func()) { t.onClose = fn }

func (t *transport) dispatch(frame []byte) {
	if len(frame) < 9 {
		return
	}
	typ := frame[0]
	id := binary.BigEndian.Uint32(frame[1:5])
	n := binary.BigEndian.Uint32(frame[5:9])
	if int(n) != len(frame)-9 {
		log.Printf("transport: bad frame, declared %d got %d", n, len(frame)-9)
		return
	}
	if t.onFrame != nil {
		t.onFrame(typ, id, frame[9:])
	}
}

func (t *transport) send(typ uint8, id uint32, payload []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return fmt.Errorf("transport closed")
	}
	frame := make([]byte, 9+len(payload))
	frame[0] = typ
	binary.BigEndian.PutUint32(frame[1:5], id)
	binary.BigEndian.PutUint32(frame[5:9], uint32(len(payload)))
	copy(frame[9:], payload)

	for t.dc.BufferedAmount() > dcHighWater {
		select {
		case <-t.lowCh:
		case <-time.After(dcSendTimeout):
			return fmt.Errorf("datachannel stuck, buffered=%d", t.dc.BufferedAmount())
		}
	}
	return t.dc.Send(frame)
}

func (t *transport) close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	t.mu.Unlock()
	if t.onClose != nil {
		t.onClose()
	}
}

// ---- stream helpers ----

// stream bridges one TCP connection over the tunnel. ch is intentionally never
// closed (a send-select on a closed channel panics); streams are torn down by
// closing conn, which makes both pump goroutines exit.
type stream struct {
	id      uint32
	conn    net.Conn
	ch      chan []byte // data destined to the TCP socket
	stopped chan struct{}
	once    sync.Once
}

func (s *stream) stop() {
	s.once.Do(func() {
		close(s.stopped)
		if s.conn != nil {
			s.conn.Close()
		}
	})
}

// connToDC pumps TCP -> DataChannel DATA frames, then FIN on EOF.
func connToDC(conn net.Conn, id uint32, send func(typ uint8, payload []byte) error) {
	buf := make([]byte, 32<<10)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if perr := send(frameData, buf[:n]); perr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	send(frameFin, nil)
}

// dcToConn pumps DataChannel DATA frames -> TCP writes.
func dcToConn(conn net.Conn, ch <-chan []byte, done func()) {
	defer func() {
		if done != nil {
			done()
		}
	}()
	for p := range ch {
		if _, err := conn.Write(p); err != nil {
			return
		}
	}
}

// registry of active streams for one tunnel
type streamReg struct {
	mu      sync.Mutex
	streams map[uint32]*stream
	next    uint32
}

func newStreamReg() *streamReg {
	return &streamReg{streams: map[uint32]*stream{}}
}

func (r *streamReg) allocID() uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	return r.next
}

func (r *streamReg) add(s *stream) {
	r.mu.Lock()
	r.streams[s.id] = s
	r.mu.Unlock()
}

func (r *streamReg) get(id uint32) *stream {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.streams[id]
}

func (r *streamReg) remove(id uint32) {
	r.mu.Lock()
	delete(r.streams, id)
	r.mu.Unlock()
}

func (r *streamReg) closeAll() {
	r.mu.Lock()
	all := make([]*stream, 0, len(r.streams))
	for _, s := range r.streams {
		all = append(all, s)
	}
	r.streams = map[uint32]*stream{}
	r.mu.Unlock()
	for _, s := range all {
		s.stop()
	}
}