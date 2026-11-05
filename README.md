# p2ptun (v0.1.7)

P2P TCP tunnel for remote SSH and TCP proxies, built on **PeerJS SCTP Control Scheduling + Native UDP (Hysteria 2 / QUIC)**.

- **调度面 (Control Plane)**: PeerJS 仅用于 SDP 交换；WebRTC SCTP DataChannel 仅用于配对认证、协议协商与原生 UDP 调度，**不经过业务数据**。
- **数据面 (Data Plane)**: 两端经调度后通过**原生裸 UDP**（基于 Pion ICE 原生打洞）建立纯 P2P 直连，彻底剔除 DTLS + SCTP 的双重加密与 1 秒 RTO 限制。
- **传输层支持 (Dual Transport)**:
  - **Hysteria 2 (`-proto hy2`，默认)**: 搭载 Brutal 拥塞控制，按设定带宽（`-bandwidth`，默认 50Mbps，可设置 100/200Mbps）恒定发包，彻底抵消跨洋高延迟弱网的丢包降速。
  - **QUIC (`-proto quic`)**: 原生 UDP 上的 QUIC (TLS 1.3) 多路复用。
  - **Classic (`-proto classic`)**: 回退到原版 WebRTC DataChannel 纯帧多路复用。

```
Client (Termux / Windows / Mac)         Server (WSL / Cloud Shell / VPS)
┌──────────────────────────────┐        ┌──────────────────────────────┐
│  127.0.0.1:1080 (or :2222)   │        │  Target TCP (:1080 / :22)    │
│  p2ptun connect              │        │  p2ptun expose               │
└──────────────┬───────────────┘        └──────────────┬───────────────┘
               │  [Control Plane] WebRTC DataChannel   │
               │  ◄════════ (SCTP 仅作调度) ════════► │
               │                                       │
               │  [Data Plane] 原生裸 UDP 直连         │
               │  ◄──────────────────────────────────► │
               │     Hysteria 2 (Brutal) / QUIC        │
               │                                       │
               │         SDP/ICE only                  │
               └──────────────► PeerJS cloud ◄─────────┘
```

## Quick Start

### 场景 1：极速跨洋代理（如 simple-http-proxy）

```sh
# 1. 服务端 (运行代理于 1080，使用 Hysteria 2 恒定 100M 发包)
p2ptun expose -id goproxy-tunnel -secret p2pproxy2026 -target 127.0.0.1:1080 -proto hy2 -bandwidth 100

# 2. 客户端 (本地电脑 / Termux)
p2ptun connect -peer goproxy-tunnel -secret p2pproxy2026 -listen 127.0.0.1:1080 -proto hy2 -bandwidth 100
```

### 场景 2：远程 SSH 穿透

```sh
# 1. 服务端 (sshd 监听 127.0.0.1:22)
p2ptun expose -id wslssh-a1b2 -secret hunter2 -target 127.0.0.1:22

# 2. 客户端 — 然后: ssh -p 2222 user@127.0.0.1
p2ptun connect -peer wslssh-a1b2 -secret hunter2 -listen 127.0.0.1:2222
```

## Flags & Environment Variables

| Flag | Env | Description |
|---|---|---|
| `-id` | `P2PTUN_ID` | Peer ID on the signaling server |
| `-secret` | `P2PTUN_SECRET` | Pairing secret, must match on both sides |
| `-peer` | `P2PTUN_PEER` | Remote peer ID (`connect`) |
| `-target` | `P2PTUN_TARGET` | Exposed TCP endpoint (`expose`), default `127.0.0.1:22` |
| `-listen` | `P2PTUN_LISTEN` | Local listen addr (`connect`), default `127.0.0.1:2222` |
| `-proto` | `P2PTUN_PROTO` | Transport protocol: `hy2` (Hysteria 2 Brutal CC, default), `quic` (Native QUIC), `classic` |
| `-bandwidth` | `P2PTUN_BANDWIDTH` | Target bandwidth in Mbps for Hysteria 2 Brutal CC (default `50`) |
| `-broker` | `P2PTUN_BROKER` | Signaling broker URL (default `wss://0.peerjs.com`) |
| `-key` | `P2PTUN_KEY` | PeerJS API key (default `peerjs`) |
| `-stun` | `P2PTUN_STUN` | Comma-separated STUN servers |
| — | `P2PTUN_DNS` | Custom DNS servers for Termux fallback |

## Build & Test

```sh
go test -v ./...
CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' .
```
