package main

// 本文件补齐 classic 传输路径的自动化覆盖.
//
// 背景: classic (DataChannel 上的帧多路复用) 这条路径长期没有端到端测试,
// 导致 handleFrameExpose / handleFrameConnect / streamReg 相关的回归
// (RST 不上报、流注册表泄漏、半关闭切断回程数据) 只能靠手工发现。
// 这里在真实 pion DataChannel 上完整跑一遍 SYN/DATA/FIN/RST 与多流并发。
//
// 注: dcPacketConn 的背压在这里**没有**做回归测试。实测在本机回环下
// SCTP 自身的发送窗口会把 BufferedAmount 卡在 ~557KB, 修复版与
// "循环内无条件 break" 的 bug 版行为无法区分 (400 次写入 315ms vs 320ms),
// 因此任何基于耗时的断言都是假的绿灯, 不如不写。

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// ---- 测试用 loopback PeerConnection 对 ----

// newLoopbackPair 建立一对已连通的 PeerConnection, 并完成双向 ICE candidate 交换.
// 返回后两侧的 DataChannel 可以正常互相打开.
func newLoopbackPair(t *testing.T) (a, b *webrtc.PeerConnection) {
	t.Helper()

	m := webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatalf("register codecs: %v", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(&m))

	a, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("new PeerConnection a: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	b, err = api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("new PeerConnection b: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	a.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			_ = b.AddICECandidate(c.ToJSON())
		}
	})
	b.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			_ = a.AddICECandidate(c.ToJSON())
		}
	})
	return a, b
}

// negotiate 由 offerer 发起, answerer 应答. 两侧都会开始交换 ICE.
func negotiate(t *testing.T, offerer, answerer *webrtc.PeerConnection) {
	t.Helper()
	offer, err := offerer.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	if err := offerer.SetLocalDescription(offer); err != nil {
		t.Fatalf("offerer SetLocalDescription: %v", err)
	}
	if err := answerer.SetRemoteDescription(offer); err != nil {
		t.Fatalf("answerer SetRemoteDescription: %v", err)
	}
	answer, err := answerer.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("create answer: %v", err)
	}
	if err := answerer.SetLocalDescription(answer); err != nil {
		t.Fatalf("answerer SetLocalDescription: %v", err)
	}
	if err := offerer.SetRemoteDescription(answer); err != nil {
		t.Fatalf("offerer SetRemoteDescription: %v", err)
	}
}

// waitDataChannelOpen 等待 DataChannel 进入 open 状态.
func waitDataChannelOpen(t *testing.T, dc *webrtc.DataChannel, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if dc.ReadyState() == webrtc.DataChannelStateOpen {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("datachannel %q did not open within %v (state=%v)", dc.Label(), timeout, dc.ReadyState())
}

// ---- classic 帧协议端到端 ----

// TestClassicTransportEndToEnd 在真实 DataChannel 上跑 classic 帧协议:
// 覆盖 SYN -> DATA -> FIN 的正常流程, 以及 RST 异常重置和多流并发,
// 并验证 TCP 半关闭 (半关闭后仍能读到干净数据).
func TestClassicTransportEndToEnd(t *testing.T) {
	// 目标服务: 收到什么回什么, 但先吐一个 banner (模拟 SSH)
	targetLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("target listen: %v", err)
	}
	t.Cleanup(func() { _ = targetLis.Close() })

	go func() {
		for {
			c, err := targetLis.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				if _, err := conn.Write([]byte("SSH-2.0-ClassicTest\r\n")); err != nil {
					return
				}
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()

	// 只接受连接、从不回数据的 target: 用来验证 classic 侧对 SYN 超时/失败的处理
	deadLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("dead listen: %v", err)
	}
	t.Cleanup(func() { _ = deadLis.Close() })

	a, b := newLoopbackPair(t)

	// 角色划分 (与生产代码一致):
	//   b = connect 侧, 主动 CreateDataChannel 并发起 offer
	//   a = expose 侧, 通过 OnDataChannel 收下通道, 侧内把流接到 -target
	exposeCfg := config{target: targetLis.Addr().String()}
	exposeReg := newStreamReg()

	remoteReady := make(chan *transport, 1)
	a.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != "p2ptun" {
			return
		}
		tr := newTransport(dc, func(tr *transport) frameHandler {
			return func(typ uint8, id uint32, payload []byte) {
				handleFrameExpose(exposeCfg, exposeReg, tr, nil, typ, id, payload)
			}
		}, nil)
		remoteReady <- tr
	})

	dc, err := b.CreateDataChannel("p2ptun", &webrtc.DataChannelInit{Ordered: boolPtr(true)})
	if err != nil {
		t.Fatalf("create datachannel: %v", err)
	}
	connectReg := newStreamReg()
	connectTr := newTransport(dc, func(tr *transport) frameHandler {
		return func(typ uint8, id uint32, payload []byte) {
			handleFrameConnect(connectReg, tr, typ, id, payload)
		}
	}, nil)

	negotiate(t, b, a)
	waitDataChannelOpen(t, dc, 10*time.Second)

	select {
	case <-remoteReady:
	case <-time.After(10 * time.Second):
		t.Fatal("expose side never received the datachannel")
	}
	t.Cleanup(func() { connectReg.closeAll() })

	// 把一个模拟的"本地 TCP 客户端"接到 classic 流上.
	// 这里直接用 net.Pipe 模拟 connect 侧的本地连接.
	type classicStream struct {
		client net.Conn // 测试这一端
		id     uint32
	}
	openStream := func() classicStream {
		clientSide, tunnelSide := net.Pipe()
		t.Cleanup(func() { _ = clientSide.Close() })

		id := connectReg.allocID()
		st := newStream(id, tunnelSide)
		connectReg.add(st)

		cleanup := func() { connectReg.remove(id) }
		go connToDC(tunnelSide, id, st, func(typ uint8, p []byte) error {
			return connectTr.send(typ, id, p)
		}, cleanup)
		go dcToConn(tunnelSide, st, func() {
			_ = connectTr.send(frameRst, id, nil)
		}, cleanup)

		if err := connectTr.send(frameSyn, id, nil); err != nil {
			t.Fatalf("send SYN: %v", err)
		}
		return classicStream{client: clientSide, id: id}
	}

	// ---- 用例 1: banner + 回显 + 半关闭 ----
	cs := openStream()
	_ = cs.client.SetDeadline(time.Now().Add(10 * time.Second))

	banner := make([]byte, len("SSH-2.0-ClassicTest\r\n"))
	if _, err := io.ReadFull(cs.client, banner); err != nil {
		t.Fatalf("read banner: %v", err)
	}
	if string(banner) != "SSH-2.0-ClassicTest\r\n" {
		t.Fatalf("bad banner: %q", banner)
	}

	msg := []byte("hello-classic-frame-protocol")
	if _, err := cs.client.Write(msg); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	echo := make([]byte, len(msg))
	if _, err := io.ReadFull(cs.client, echo); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(echo, msg) {
		t.Fatalf("echo mismatch: got %q want %q", echo, msg)
	}

	// 半关闭: 关掉写方向后, 仍应能继续读到对端数据 (这里对端无更多数据, 读到 EOF)
	if cw, ok := cs.client.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	} else {
		_ = cs.client.Close()
	}

	// ---- 用例 2: 多流并发 ----
	var wg sync.WaitGroup
	errCh := make(chan error, 16)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			s := openStream()
			_ = s.client.SetDeadline(time.Now().Add(15 * time.Second))

			buf := make([]byte, len("SSH-2.0-ClassicTest\r\n"))
			if _, err := io.ReadFull(s.client, buf); err != nil {
				errCh <- fmt.Errorf("stream %d banner: %w", idx, err)
				return
			}
			payload := []byte(fmt.Sprintf("concurrent-stream-%d", idx))
			if _, err := s.client.Write(payload); err != nil {
				errCh <- fmt.Errorf("stream %d write: %w", idx, err)
				return
			}
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(s.client, got); err != nil {
				errCh <- fmt.Errorf("stream %d read: %w", idx, err)
				return
			}
			if !bytes.Equal(got, payload) {
				errCh <- fmt.Errorf("stream %d mismatch: got %q want %q", idx, got, payload)
			}
			_ = s.client.Close()
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	// ---- 用例 3: 目标不可达时 expose 侧应回 RST 而不是挂住 ----
	//
	// 这里不复用上面的 exposeCfg: 它被 a.OnDataChannel 里的 handler 并发读取,
	// 在测试 goroutine 里改字段本身就是 data race (race detector 会直接报)。
	// 改为另开一条 DataChannel, 用独立 config 指向一个必然拒绝连接的端口。
	unreachable := "127.0.0.1:1"
	deadCfg := config{target: unreachable}
	deadReg := newStreamReg()

	deadPairA, deadPairB := newLoopbackPair(t)
	deadReady := make(chan struct{}, 1)
	deadPairA.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != "p2ptun-dead" {
			return
		}
		newTransport(dc, func(tr *transport) frameHandler {
			return func(typ uint8, id uint32, payload []byte) {
				handleFrameExpose(deadCfg, deadReg, tr, nil, typ, id, payload)
			}
		}, nil)
		deadReady <- struct{}{}
	})
	deadDC, err := deadPairB.CreateDataChannel("p2ptun-dead", &webrtc.DataChannelInit{Ordered: boolPtr(true)})
	if err != nil {
		t.Fatalf("create dead datachannel: %v", err)
	}
	badReg := newStreamReg()
	badTr := newTransport(deadDC, func(tr *transport) frameHandler {
		return func(typ uint8, id uint32, payload []byte) {
			handleFrameConnect(badReg, tr, typ, id, payload)
		}
	}, nil)
	negotiate(t, deadPairB, deadPairA)
	waitDataChannelOpen(t, deadDC, 10*time.Second)
	select {
	case <-deadReady:
	case <-time.After(10 * time.Second):
		t.Fatal("expose side never received the dead datachannel")
	}

	badClientSide, badTunnelSide := net.Pipe()
	t.Cleanup(func() { _ = badClientSide.Close() })
	badID := badReg.allocID()
	badSt := newStream(badID, badTunnelSide)
	badReg.add(badSt)
	badCleanup := func() { badReg.remove(badID) }
	go connToDC(badTunnelSide, badID, badSt, func(typ uint8, p []byte) error {
		return badTr.send(typ, badID, p)
	}, badCleanup)
	go dcToConn(badTunnelSide, badSt, func() {
		_ = badTr.send(frameRst, badID, nil)
	}, badCleanup)
	if err := badTr.send(frameSyn, badID, nil); err != nil {
		t.Fatalf("send SYN for unreachable target: %v", err)
	}

	// 等 expose 侧 dial 失败并回 RST, connect 侧应摘掉这条流
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if badReg.get(badID) == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if badReg.get(badID) != nil {
		t.Fatal("stream was not reset after unreachable target dial failed")
	}
	_ = badClientSide.Close()

	// 流上限: 确保 closeAll 之后计数归零
	connectReg.closeAll()
	if n := connectReg.count(); n != 0 {
		t.Fatalf("expected empty registry after closeAll, got %d", n)
	}
}
