package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

// DataChannel frame types (multiplex multiple TCP streams over one DC)
const (
	frameSyn  = 1
	frameData = 2
	frameFin  = 3
	framePing = 4 // 数据通道内保活探测 (带 8 字节纳秒时间戳, 向后兼容旧版)
	framePong = 5 // 数据通道内保活应答
	frameRst  = 6 // 异常重置关闭流 (例如目标端口连接失败)

	dcHighWater       = 4 << 20 // pause sending above 4MB buffered
	dcLowWater        = 1 << 20 // resume after drops below 1MB
	dcSendTimeout     = 60 * time.Second
	streamEnqueueWait = 15 * time.Second
	maxChunkSize      = 32 << 10
	// maxStreamsPerSession caps concurrent streams per tunnel session so a peer
	// cannot exhaust file descriptors by flooding frameSyn (each pending dial holds one FD).
	maxStreamsPerSession = 256
)

type frameHandler func(typ uint8, id uint32, payload []byte)

// transport wraps a WebRTC DataChannel with frame framing, flow control, and keepalive.
type transport struct {
	dc    *webrtc.DataChannel
	lowCh chan struct{}

	onFrame frameHandler
	onClose func()

	mu        sync.Mutex
	closed    bool
	closeOnce sync.Once
	done      chan struct{}

	lastRecvNs  atomic.Int64
	lastRTTNs   atomic.Int64
	peerHasPong atomic.Bool
	bytesTx     atomic.Uint64
	bytesRx     atomic.Uint64
}

// newTransport builds the transport and wires DataChannel callbacks.
//
// makeHandler is called with the transport instance BEFORE dc.OnMessage is
// registered, so the frame handler never has to look the transport up through
// session state (which is assigned only after newTransport returns). This fixes
// the OnOpen/first-frame race where the first frameSyn could be dropped.
func newTransport(dc *webrtc.DataChannel, makeHandler func(t *transport) frameHandler, onClose func()) *transport {
	t := &transport{
		dc:    dc,
		lowCh: make(chan struct{}, 1),
		done:  make(chan struct{}),
	}
	t.lastRecvNs.Store(time.Now().UnixNano())
	dc.SetBufferedAmountLowThreshold(dcLowWater)
	dc.OnBufferedAmountLow(func() {
		select {
		case t.lowCh <- struct{}{}:
		default:
		}
	})
	if makeHandler != nil {
		t.onFrame = makeHandler(t)
	}
	t.onClose = onClose
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		t.dispatch(msg.Data)
	})
	dc.OnClose(func() {
		t.close()
	})
	dc.OnError(func(err error) {
		log.Printf("transport: datachannel error: %v", err)
		t.close()
	})
	return t
}

func (t *transport) setOnClose(fn func()) {
	t.mu.Lock()
	t.onClose = fn
	t.mu.Unlock()
}

// startKeepalive 在 DataChannel 上定期发送 framePing 保活 UDP NAT 映射并检测死连接
func (t *transport) startKeepalive(interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		timeout := interval * 3
		if timeout < 25*time.Second {
			timeout = 25 * time.Second
		}
		for {
			select {
			case <-t.done:
				return
			case <-ticker.C:
				// 如果对端支持 Ping/Pong 且超过 3 个周期未收到任何帧, 判定连接已断 (如网络切换)
				if t.peerHasPong.Load() {
					last := time.Unix(0, t.lastRecvNs.Load())
					if time.Since(last) > timeout {
						log.Printf("transport: keepalive timeout (no frame for %v), closing", time.Since(last).Round(time.Second))
						t.close()
						return
					}
				}
				var ts [8]byte
				binary.BigEndian.PutUint64(ts[:], uint64(time.Now().UnixNano()))
				if err := t.send(framePing, 0, ts[:]); err != nil {
					t.close()
					return
				}
			}
		}
	}()
}

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
	payload := frame[9:]
	t.lastRecvNs.Store(time.Now().UnixNano())
	t.bytesRx.Add(uint64(len(payload)))
	globalStats.bytesRx.Add(uint64(len(payload)))

	// 控制帧直接在 transport 层处理, 避免被单个阻塞的数据流卡住
	switch typ {
	case framePing:
		t.peerHasPong.Store(true)
		_ = t.send(framePong, id, payload)
		return
	case framePong:
		t.peerHasPong.Store(true)
		if len(payload) >= 8 {
			sent := int64(binary.BigEndian.Uint64(payload[:8]))
			rtt := time.Now().UnixNano() - sent
			if rtt > 0 && rtt < int64(time.Minute) {
				t.lastRTTNs.Store(rtt)
				globalStats.lastRTTMs.Store(rtt / int64(time.Millisecond))
			}
		}
		return
	}

	if t.onFrame != nil {
		t.onFrame(typ, id, payload)
	}
}

func (t *transport) send(typ uint8, id uint32, payload []byte) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return fmt.Errorf("transport closed")
	}
	frame := make([]byte, 9+len(payload))
	frame[0] = typ
	binary.BigEndian.PutUint32(frame[1:5], id)
	binary.BigEndian.PutUint32(frame[5:9], uint32(len(payload)))
	copy(frame[9:], payload)

	// Only DATA frames participate in flow control: control frames (PING/PONG/
	// FIN/RST) are tiny and are sent from the DataChannel OnMessage goroutine.
	// Blocking that goroutine on backpressure would deadlock the whole receive
	// path (we could never drain the buffer that unblocks us).
	if typ == frameData {
		for t.dc.BufferedAmount() > dcHighWater {
			t.mu.Unlock()
			select {
			case <-t.lowCh:
				// Cascade wakeup: lowCh holds one token but OnBufferedAmountLow is
				// edge-triggered. Re-post the token while we are still below the
				// high-water mark so the other waiting streams are released too.
				if t.dc.BufferedAmount() <= dcHighWater {
					select {
					case t.lowCh <- struct{}{}:
					default:
					}
				}
			case <-t.done:
				return fmt.Errorf("transport closed while waiting backpressure")
			case <-time.After(dcSendTimeout):
				t.close()
				return fmt.Errorf("datachannel stuck, buffered=%d", t.dc.BufferedAmount())
			}
			t.mu.Lock()
			if t.closed {
				t.mu.Unlock()
				return fmt.Errorf("transport closed")
			}
		}
	}
	err := t.dc.Send(frame)
	t.mu.Unlock()
	if err == nil {
		t.bytesTx.Add(uint64(len(payload)))
		globalStats.bytesTx.Add(uint64(len(payload)))
	}
	return err
}

func (t *transport) close() { t.closeInner(true) }

// closeNoNotify closes the transport without invoking the onClose callback.
// Use it from a caller that is itself the onClose target (e.g. exposeSession.close,
// which is registered via setOnClose): calling close() there would re-enter
// sync.Once.Do on the same Once and deadlock.
func (t *transport) closeNoNotify() { t.closeInner(false) }

func (t *transport) closeInner(notify bool) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	close(t.done)
	fn := t.onClose
	t.mu.Unlock()
	_ = t.dc.Close()
	if notify && fn != nil {
		t.closeOnce.Do(fn)
	}
}

// ---- stream helpers ----

// stream bridges one TCP connection over the tunnel.
// 修复了原版的 4 个并发与协议漏洞:
//  1. dcToConn 之前未监听 <-st.stopped, 导致会话中断或异常关闭时 goroutine 永久泄漏;
//  2. expose 侧异步 DialTimeout 期间若收到 FIN 或 stop, 之前未原子检查直接覆盖 st.conn 导致连接泄漏;
//  3. 单个慢速流 ch 满时会阻塞整个 DataChannel OnMessage 回调, 现增加安全入队超时保护;
//  4. 收到对端 FIN 排空队列后, 若直接 conn.Close() 会把己方正在读取响应的 connToDC 一并掐断!
//     现优先调用 TCPConn.CloseWrite() 执行真正的 TCP 半关闭 (SHUT_WR), 待双向读写都结束后再完全关闭 socket。
type stream struct {
	id          uint32
	connMu      sync.Mutex
	conn        net.Conn
	isStopped   bool
	activePumps atomic.Int32
	ch          chan []byte // data destined to the TCP socket
	stopped     chan struct{}
	finCh       chan struct{} // closed when FIN received -> drain ch, then half-close write
	once        sync.Once
	finOnce     sync.Once
}

func newStream(id uint32, conn net.Conn) *stream {
	if conn != nil {
		tuneTCPConn(conn)
	}
	st := &stream{
		id:      id,
		conn:    conn,
		ch:      make(chan []byte, 1024),
		stopped: make(chan struct{}),
		finCh:   make(chan struct{}),
	}
	st.activePumps.Store(2)
	return st
}

// attachConn 原子绑定异步建立的 TCP 连接; 若流已提前关闭则立即关掉 conn 返回 false
func (s *stream) attachConn(conn net.Conn) bool {
	tuneTCPConn(conn)
	s.connMu.Lock()
	if s.isStopped {
		s.connMu.Unlock()
		_ = conn.Close()
		return false
	}
	s.conn = conn
	s.connMu.Unlock()
	return true
}

// pumpDone 在 connToDC 或 dcToConn 退出时调用; 当双向协程均退出后关闭底层 socket
func (s *stream) pumpDone(onBothDone func()) {
	if s.activePumps.Add(-1) <= 0 {
		s.stop()
		if onBothDone != nil {
			onBothDone()
		}
	}
}

func (s *stream) stop() {
	s.once.Do(func() {
		s.connMu.Lock()
		s.isStopped = true
		c := s.conn
		s.connMu.Unlock()
		close(s.stopped)
		if c != nil {
			_ = c.Close()
		}
	})
}

// halfClose signals "no more DATA frames will arrive on this stream"; the
// DC->TCP pump drains the remaining ch without dropping bytes, then half-closes conn.
func (s *stream) halfClose() {
	s.finOnce.Do(func() { close(s.finCh) })
}

// enqueue 将收到的 DATA 帧安全送入流队列, 避免单个卡死流永久阻塞整个隧道的接收协程
func (s *stream) enqueue(payload []byte) bool {
	cp := make([]byte, len(payload))
	copy(cp, payload)
	select {
	case s.ch <- cp:
		return true
	case <-s.stopped:
		return false
	default:
		timer := time.NewTimer(streamEnqueueWait)
		defer timer.Stop()
		select {
		case s.ch <- cp:
			return true
		case <-s.stopped:
			return false
		case <-timer.C:
			log.Printf("stream %d: local TCP reader stalled >%v, resetting stream", s.id, streamEnqueueWait)
			s.stop()
			return false
		}
	}
}

// connToDC pumps TCP -> DataChannel DATA frames, then FIN on EOF.
func connToDC(conn net.Conn, id uint32, st *stream, send func(typ uint8, payload []byte) error, onBothDone func()) {
	defer st.pumpDone(onBothDone)
	buf := make([]byte, maxChunkSize)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if perr := send(frameData, buf[:n]); perr != nil {
				st.stop()
				return
			}
		}
		if err != nil {
			break
		}
	}
	_ = send(frameFin, nil)
}

// dcToConn pumps DataChannel DATA frames -> TCP writes. On FIN it first drains
// everything already queued in ch, writes it to conn, and then half-closes the write
// side (CloseWrite) so connToDC can still finish reading any remaining response bytes.
func dcToConn(conn net.Conn, st *stream, onWriteErr func(), onBothDone func()) {
	defer st.pumpDone(onBothDone)
	write := func(p []byte) bool {
		for len(p) > 0 {
			n, err := conn.Write(p)
			if err != nil {
				if onWriteErr != nil {
					onWriteErr()
				}
				st.stop()
				return false
			}
			p = p[n:]
		}
		return true
	}
	closeWriteHalf := func() {
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = conn.Close()
		}
	}
	for {
		select {
		case <-st.stopped:
			return
		case p := <-st.ch:
			if !write(p) {
				return
			}
		case <-st.finCh:
			for {
				select {
				case <-st.stopped:
					return
				case p := <-st.ch:
					if !write(p) {
						return
					}
				default:
					closeWriteHalf()
					return
				}
			}
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
	if r.next == 0 {
		r.next = 1
	}
	return r.next
}

func (r *streamReg) add(s *stream) {
	r.mu.Lock()
	_, existed := r.streams[s.id]
	r.streams[s.id] = s
	r.mu.Unlock()
	if !existed {
		globalStats.activeStreams.Add(1)
	}
}

func (r *streamReg) get(id uint32) *stream {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.streams[id]
}

func (r *streamReg) remove(id uint32) *stream {
	r.mu.Lock()
	s := r.streams[id]
	_, existed := r.streams[id]
	delete(r.streams, id)
	r.mu.Unlock()
	if existed {
		globalStats.activeStreams.Add(-1)
	}
	return s
}

func (r *streamReg) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.streams)
}

func (r *streamReg) closeAll() {
	r.mu.Lock()
	all := make([]*stream, 0, len(r.streams))
	for _, s := range r.streams {
		all = append(all, s)
	}
	r.streams = map[uint32]*stream{}
	r.mu.Unlock()
	if len(all) > 0 {
		globalStats.activeStreams.Add(-int64(len(all)))
	}
	for _, s := range all {
		s.stop()
	}
}
