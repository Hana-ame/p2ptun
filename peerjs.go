package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
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

// pjSignal is the payload of OFFER / ANSWER / CANDIDATE messages.
type pjSignal struct {
	SDP           pjSDP       `json:"sdp"`
	Type          string      `json:"type"`
	ConnectionID  string      `json:"connectionId"`
	Label         string      `json:"label"`
	Metadata      string      `json:"metadata"`
	Reliable      *bool       `json:"reliable"`
	Serialization string      `json:"serialization"`
	Candidate     pjIce   `json:"candidate"`
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

	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

var errClosed = errors.New("signaling closed")

// dialSignaling connects to a PeerJS cloud/server WebSocket and waits for OPEN.
func dialSignaling(broker, key, id, token string, onSignal func(string, string, pjSignal), onClose func(error)) (*signalingClient, error) {
	u := broker + "/peerjs?key=" + url.QueryEscape(key) +
		"&id=" + url.QueryEscape(id) +
		"&token=" + url.QueryEscape(token) +
		"&version=1.5.2"

	h := http.Header{}
	h.Set("Origin", "https://peerjs.com")
	h.Set("User-Agent", "Mozilla/5.0 (p2ptun)")

	ws, resp, err := websocket.DefaultDialer.Dial(u, h)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("dial: %v (http %d)", err, resp.StatusCode)
		}
		return nil, fmt.Errorf("dial: %w", err)
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

	// Wait for OPEN / ERROR / ID-TAKEN before returning.
	ws.SetReadDeadline(time.Now().Add(15 * time.Second))
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			ws.Close()
			return nil, fmt.Errorf("waiting OPEN: %w", err)
		}
		var m pjMessage
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		switch m.Type {
		case pjOpen:
			ws.SetReadDeadline(time.Time{})
			go sc.readLoop()
			go sc.heartbeatLoop()
			return sc, nil
		case pjError, pjIDTaken:
			ws.Close()
			return nil, fmt.Errorf("server refused: %s %s", m.Type, string(m.Payload))
		}
	}
}

func (sc *signalingClient) readLoop() {
	for {
		_, data, err := sc.ws.ReadMessage()
		if err != nil {
			sc.onClose(err)
			return
		}
		var m pjMessage
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		switch m.Type {
		case pjOffer, pjAnswer, pjCandidate:
			var sig pjSignal
			if err := json.Unmarshal(m.Payload, &sig); err != nil {
				log.Printf("peerjs: bad payload from %s: %v", m.Src, err)
				continue
			}
			if sc.onSignal != nil {
				sc.onSignal(m.Type, m.Src, sig)
			}
		case pjError:
			log.Printf("peerjs: server error: %s", string(m.Payload))
		}
	}
}

func (sc *signalingClient) heartbeatLoop() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-sc.done:
			return
		case <-t.C:
			if err := sc.send(pjHeartbeat, "", nil); err != nil {
				return
			}
		}
	}
}

func (sc *signalingClient) send(typ, dst string, payload interface{}) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.closed {
		return errClosed
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	m := pjMessage{Type: typ, Dst: dst, Payload: b}
	return sc.ws.WriteJSON(m)
}

// sendSignal sends OFFER / ANSWER / CANDIDATE with a pjSignal payload.
func (sc *signalingClient) sendSignal(typ, dst string, sig pjSignal) error {
	return sc.send(typ, dst, sig)
}

func (sc *signalingClient) shutdown() {
	sc.mu.Lock()
	if sc.closed {
		sc.mu.Unlock()
		return
	}
	sc.closed = true
	sc.ws.Close()
	close(sc.done)
	sc.mu.Unlock()
}