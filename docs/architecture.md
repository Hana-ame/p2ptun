# p2ptun 架构

设计目标：**远程 SSH 全程 P2P 直连；信令服务器（PeerJS cloud）只交换 SDP/ICE，
绝不经过业务数据。无 TURN、无中继——这是硬约束，不是缺口。**

```
┌─ remote (Termux / any) ─┐        ┌─ WSL / service owner ─┐
│                          │        │                        │
│  ssh -p2222 127.0.0.1    │        │  sshd :22              │
│        │                 │        │        ▲               │
│  p2ptun connect :2222    │        │  p2ptun expose         │
│        ▾                 │        │        │               │
│  WebRTC DataChannel ◄────┼────────┼──────►│                │
│  (P2P, ICE + STUN)       │        │        │               │
└────────┬─────────────────┘        └────────┼───────────────┘
         │     SDP / ICE candidates only     │
         └──────────────► PeerJS cloud ◄─────┘
```

## 1. 信令层（PeerJS cloud, `pkg` 内 `peerjs.go`）

两端各开一条 WebSocket 连到 `wss://0.peerjs.com/peerjs?key=peerjs&id=..&token=..`，
实现 PeerJS 协议子集：

| 消息 | 用途 |
|---|---|
| `OPEN` | 注册 peer id（token 随机生成，仅信令会话用） |
| `HEARTBEAT` | 保活，心跳循环常驻（见 `heartbeatLoop`） |
| `OFFER` | connect 侧发起：本地 SDP **+ metadata 里带配对 secret** |
| `ANSWER` | expose 侧回 SDP；**secret 不匹配直接拒绝 OFFER** |
| `CANDIDATE` | 双向转发 ICE candidate |

- secret 只在 OFFER metadata 里过信令，且 PeerJS cloud 本身就转发整个 JSON——
  所以 secret 防的是"误连/撞 id"，不是防信令服务器偷看（我们信任该服务器只做信令）。
- 本实现**只实现 TCP over 单 DataChannel**（PeerJS 的 `DataConnection` 语义）。

## 2. 数据面（`bridge.go`：单 DataChannel 多路复用）

不用每连一次 SSH 都建一条 PeerConnection；一条 DataChannel 复用在上面：

```
9-字节帧头 + payload
┌──────┬────────────┬────────────┬─────────────┐
│ type │  connID(4) │ length(4)  │   payload   │
│ 1B   │             BE, uint32   │             │
└──────┴────────────┴────────────┴─────────────┘

type: 1=SYN(新流)  2=DATA  3=FIN(对端已关)
```

- 每次本地 TCP accept → 发 `SYN(connID)` → 双向 `DATA` → EOF 时 `FIN`。
- 发侧看 `bufferedAmount` 做背压：>4MB 暂停读 TCP，回落到 1MB 再继续
  （`dcHighWater` / `dcLowWater`），避免内存无上限堆积。

## 3. 会话生命周期（`main.go`）

- **expose**：常驻。信令就绪后等 OFFER → 校验 secret → ANSWER →
  建 PC → DataChannel 事件 → 循环 accept TCP 流。会话失败后等 3s 重来
  （`signaling ready` → `OFFER` → …，全程日志可见）。
- **connect**：常驻。信令就绪后立即发 OFFER；本地监听 `127.0.0.1:2222`，
  accept 到的每个连接都映射成一条远端 TCP 流。失败同样 3s 重试。
- **ICE**：candidate 可能在远端 setRemoteDescription 之前就到，
  `sdpState` 先缓冲，`onRemoteSet` 后再逐条 `AddICECandidate`。
- 收尾：`pc state=closed` 是 teardown 里主动 `pc.Close()` 的正常日志，
  **不是错误**——它常紧跟 `session ended: ... (retry in 3s)` 出现。

## 4. 网络修复（`netfix.go`）

Termux/Android 两个坑，程序启动时自动处理：

1. Termux 无 `/etc/resolv.conf`：固定公共 DNS
   `223.5.5.5 → 114.114.114.114 → 8.8.8.8 → 1.1.1.1`（`P2PTUN_DNS` 覆盖），
   UDP 53 全不通时自动回退 TCP 53。
2. Termux CA 在 `$PREFIX/etc/tls/cert.pem`：附加进 websocket TLS 根池。

proxy 环境变量（`http_proxy` 等）启动时全部 `os.Unsetenv`，不接受代理干扰。

## 5. 已知边界（与设计目标一致）

- 纯 UDP 打洞（STUN srflx）。任一侧 UDP 出站被过滤（如某些公网/Wi-Fi 环境）、
  或是对称 NAT，P2P 会失败。**此时唯一解法是 TURN 中继——这违背"不转发"约束，
  本项目明确不接受**（`docs/troubleshooting.md` 有判断方法）。
- 目录结构：

```
main.go       子命令/配置/会话循环/sdpState
peerjs.go     PeerJS cloud 信令客户端
bridge.go     DataChannel 多路复用帧 + 背压
netfix.go     Termux DNS/CA 修复
```