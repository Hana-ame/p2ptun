package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// PeerJS cloud message types (peerjs-server src/enums.ts)
const (
	pjOpen      = "OPEN"
	pjOffer     = "OFFER"
	pjAnswer    = "ANSWER"
	pjCandidate = "CANDIDATE"
	pjLeave     = "LEAVE"
	pjHeartbeat = "HEARTBEAT"
	pjError     = "ERROR"
	pjIDTaken   = "ID-TAKEN"

	wsHeartbeatInterval = 5 * time.Second
	wsReadTimeout       = 25 * time.Second
	wsWriteTimeout      = 10 * time.Second
)

type pjSDP struct {
	Type string `json:"type"`
	SDP  string `json:"sdp"`
}

type pjIce struct {
	Candidate        string `json:"candidate"`
	SDPMid           string `json:"sdpMid"`
	SDPMLineIndex    uint16 `json:"sdpMLineIndex"`
	UsernameFragment string `json:"usernameFragment"`
}

// pjSignal is the payload of OFFER / ANSWER / CANDIDATE / LEAVE messages.
type pjSignal struct {
	SDP           pjSDP  `json:"sdp"`
	Type          string `json:"type"`
	ConnectionID  string `json:"connectionId"`
	Label         string `json:"label"`
	Metadata      string `json:"metadata"`
	Reliable      *bool  `json:"reliable"`
	Serialization string `json:"serialization"`
	Candidate     pjIce  `json:"candidate"`
}

// pjMessage is the wire envelope: {type, src, dst, payload}.
// src is injected by the PeerJS server, not by the sender.
type pjMessage struct {
	Type    string          `json:"type"`
	Src     string          `json:"src,omitempty"`
	Dst     string          `json:"dst,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type signalingClient struct {
	ws     *websocket.Conn
	broker string
	key    string
	id     string
	token  string

	onSignal func(msgType, src string, sig pjSignal)
	onClose  func(err error)

	mu        sync.Mutex
	closed    bool
	closeOnce sync.Once
	done      chan struct{}
}

var (
	errClosed  = errors.New("signaling closed")
	errIDTaken = errors.New("peer id already taken on signaling server")
)

// buildPeerJSURL 规范化 broker 地址并拼接 /peerjs 查询参数
func buildPeerJSURL(broker, key, id, token string) string {
	b := strings.TrimSpace(broker)
	b = strings.TrimRight(b, "/")
	if strings.HasPrefix(b, "https://") {
		b = "wss://" + strings.TrimPrefix(b, "https://")
	} else if strings.HasPrefix(b, "http://") {
		b = "ws://" + strings.TrimPrefix(b, "http://")
	} else if !strings.Contains(b, "://") {
		b = "wss://" + b
	}
	if !strings.HasSuffix(b, "/peerjs") {
		b = b + "/peerjs"
	}
	return b + "?key=" + url.QueryEscape(key) +
		"&id=" + url.QueryEscape(id) +
		"&token=" + url.QueryEscape(token) +
		"&version=1.5.2"
}

// contextWithStops 返回一个在任一 stop 通道关闭（或外部 cancel）时自动取消的 context。
// 用于让信令握手 / 拨号这类阻塞操作能在 /quit、SIGTERM、隧道会话结束时立即中止，
// 而不是干等 websocket/ICE 超时（实测可达 6~15s，会让 p2ptun stop 误判为未退出而强杀）。
func contextWithStops(stop1, stop2 <-chan struct{}) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-stop1:
		case <-stop2:
		case <-ctx.Done():
			return
		}
		cancel()
	}()
	return ctx, cancel
}

// dialWebSocketCtx 在 ctx 取消时立即返回（即使底层 websocket 握手/TLS/代理拨号
// 自身不监听 ctx）。gorilla 的 DialContext 只在 TCP 拨号阶段看 ctx，握手阶段
// 最长会卡 HandshakeTimeout(15s)；这里并发等待，保证 /quit 能秒退。
func dialWebSocketCtx(ctx context.Context, d *websocket.Dialer, u string, h http.Header) (*websocket.Conn, *http.Response, error) {
	type dialResult struct {
		conn *websocket.Conn
		resp *http.Response
		err  error
	}
	ch := make(chan dialResult, 1)
	go func() {
		conn, resp, err := d.DialContext(ctx, u, h)
		ch <- dialResult{conn, resp, err}
	}()
	select {
	case r := <-ch:
		return r.conn, r.resp, r.err
	case <-ctx.Done():
		// 底层拨号可能稍后才返回; 拿到连接就关掉, 避免泄漏.
		go func() {
			r := <-ch
			if r.conn != nil {
				_ = r.conn.Close()
			}
		}()
		return nil, nil, ctx.Err()
	}
}

// dialSignalingAny 支持多 broker 列表依次容灾连接, 并在直连失败且存在环境代理时自动尝试代理回退
func dialSignalingAny(ctx context.Context, brokers []string, key, id, token string, onSignal func(string, string, pjSignal), onClose func(error)) (*signalingClient, error) {
	if len(brokers) == 0 {
		brokers = []string{defaultBroker}
	}
	var lastErr error
	for _, b := range brokers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sc, err := dialSignaling(ctx, b, key, id, token, onSignal, onClose)
		if err == nil {
			return sc, nil
		}
		lastErr = err
		// 如果是 ID-TAKEN, 换 broker 会导致两端不在同一信令服, 直接返回由上层处理等待或换 ID
		if errors.Is(err, errIDTaken) {
			return nil, err
		}
	}
	return nil, lastErr
}

// dialSignaling connects to a PeerJS cloud/server WebSocket and waits for OPEN.
// ctx 取消（/quit、SIGTERM、会话结束）时立即中断握手，不再阻塞到超时。
func dialSignaling(ctx context.Context, broker, key, id, token string, onSignal func(string, string, pjSignal), onClose func(error)) (*signalingClient, error) {
	u := buildPeerJSURL(broker, key, id, token)

	h := http.Header{}
	h.Set("Origin", "https://peerjs.com")
	h.Set("User-Agent", "Mozilla/5.0 (p2ptun/"+version+")")

	ws, resp, err := dialWebSocketCtx(ctx, wsDialer(), u, h)
	if err != nil && configuredProxy == "" && !keepAmbientProxy && ambientProxy != "" && ctx.Err() == nil {
		// 直连信令失败时, 若启动前环境带有代理 (如国内环境需代理连 0.peerjs.com), 自动尝试代理连接信令
		log.Printf("peerjs: direct dial %s failed (%v), trying fallback proxy %s", broker, err, ambientProxy)
		ws, resp, err = dialWebSocketCtx(ctx, wsDialerWithProxy(ambientProxy), u, h)
	}
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("dial %s: %v (http %d)", broker, err, resp.StatusCode)
		}
		return nil, fmt.Errorf("dial %s: %w", broker, err)
	}

	sc := &signalingClient{
		ws:       ws,
		broker:   broker,
		key:      key,
		id:       id,
		token:    token,
		onSignal: onSignal,
		onClose:  onClose,
		done:     make(chan struct{}),
	}

	// 握手等待 OPEN 期间也要能被 ctx 打断, 否则 /quit 会卡到 15s read deadline.
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-ctx.Done():
			_ = ws.Close()
		case <-stopWatch:
		}
	}()

	// Wait for OPEN / ERROR / ID-TAKEN before returning.
	_ = ws.SetReadDeadline(time.Now().Add(15 * time.Second))
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			ws.Close()
			return nil, fmt.Errorf("waiting OPEN from %s: %w", broker, err)
		}
		var m pjMessage
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		switch m.Type {
		case pjOpen:
			_ = ws.SetReadDeadline(time.Now().Add(wsReadTimeout))
			ws.SetPongHandler(func(string) error {
				_ = ws.SetReadDeadline(time.Now().Add(wsReadTimeout))
				return nil
			})
			ws.SetPingHandler(func(appData string) error {
				_ = ws.SetReadDeadline(time.Now().Add(wsReadTimeout))
				_ = ws.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(wsWriteTimeout))
				return nil
			})
			go sc.readLoop()
			go sc.heartbeatLoop()
			return sc, nil
		case pjIDTaken:
			ws.Close()
			return nil, fmt.Errorf("%w: %s %s", errIDTaken, m.Type, string(m.Payload))
		case pjError:
			ws.Close()
			if strings.Contains(strings.ToUpper(string(m.Payload)), "TAKEN") {
				return nil, fmt.Errorf("%w: %s %s", errIDTaken, m.Type, string(m.Payload))
			}
			return nil, fmt.Errorf("server refused: %s %s", m.Type, string(m.Payload))
		}
	}
}

func (sc *signalingClient) notifyClose(err error) {
	sc.closeOnce.Do(func() {
		if sc.onClose != nil {
			sc.onClose(err)
		}
	})
}

func (sc *signalingClient) readLoop() {
	for {
		_, data, err := sc.ws.ReadMessage()
		if err != nil {
			sc.shutdown()
			sc.notifyClose(err)
			return
		}
		_ = sc.ws.SetReadDeadline(time.Now().Add(wsReadTimeout))
		var m pjMessage
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		switch m.Type {
		case pjOffer, pjAnswer, pjCandidate, pjLeave:
			var sig pjSignal
			if len(m.Payload) > 0 {
				if err := json.Unmarshal(m.Payload, &sig); err != nil {
					log.Printf("peerjs: bad payload from %s (type=%s): %v", m.Src, m.Type, err)
					continue
				}
			}
			if sc.onSignal != nil {
				sc.onSignal(m.Type, m.Src, sig)
			}
		case pjHeartbeat:
			// Server heartbeat ack
		case pjError:
			log.Printf("peerjs: server error: %s", string(m.Payload))
		}
	}
}

func (sc *signalingClient) heartbeatLoop() {
	t := time.NewTicker(wsHeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-sc.done:
			return
		case <-t.C:
			// 同时发送 PeerJS 协议层 HEARTBEAT 与 WebSocket 控制帧 Ping,
			// 确保 Cloudflare/PeerJS 服务端保活且能触发本地 PongHandler 刷新 ReadDeadline
			if err := sc.send(pjHeartbeat, "", nil); err != nil {
				sc.shutdown()
				sc.notifyClose(err)
				return
			}
			sc.mu.Lock()
			if !sc.closed {
				_ = sc.ws.WriteControl(websocket.PingMessage, []byte("hb"), time.Now().Add(wsWriteTimeout))
			}
			sc.mu.Unlock()
		}
	}
}

func (sc *signalingClient) send(typ, dst string, payload interface{}) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.closed {
		return errClosed
	}
	var b json.RawMessage
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		b = raw
	}
	m := pjMessage{Type: typ, Dst: dst, Payload: b}
	_ = sc.ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return sc.ws.WriteJSON(m)
}

// sendSignal sends OFFER / ANSWER / CANDIDATE / LEAVE with a pjSignal payload.
func (sc *signalingClient) sendSignal(typ, dst string, sig pjSignal) error {
	return sc.send(typ, dst, sig)
}

// sendLeave 主动通知对端会话结束, 便于对端立即释放 PeerConnection
func (sc *signalingClient) sendLeave(dst, connID string) {
	if dst == "" {
		return
	}
	_ = sc.sendSignal(pjLeave, dst, pjSignal{
		Type:         "data",
		ConnectionID: connID,
	})
}

func (sc *signalingClient) isClosed() bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.closed
}

func (sc *signalingClient) shutdown() {
	sc.mu.Lock()
	if sc.closed {
		sc.mu.Unlock()
		return
	}
	sc.closed = true
	_ = sc.ws.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(500*time.Millisecond),
	)
	_ = sc.ws.Close()
	close(sc.done)
	sc.mu.Unlock()
}
