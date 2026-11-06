# p2ptun (v0.5.1)

P2P TCP tunnel for remote SSH and TCP proxies, featuring **PeerJS SCTP Control Scheduling + Native ICE UDP Direct Punching (Hysteria 2 / QUIC)** with **Enterprise Multi-Peer Daemonization & Security Hardening**.

- **调度面 (Control Plane)**: PeerJS (`wss://0.peerjs.com,wss://1.peerjs.com` 多服容灾) 交换 SDP；WebRTC SCTP DataChannel (`p2ptun-control`) 仅用于配对认证、协议协商与原生 UDP 候选地址调度，**不经过业务数据**。
- **数据面 (Data Plane)**: 两端经 SCTP 调度后通过 **原生裸 UDP**（基于 `pion/ice/v4` 原生打洞）建立纯 P2P 直连，彻底剔除 DTLS + SCTP 的双重加密与 1 秒 RTO 限制：
  - **Hysteria 2 (`-proto hy2`，默认)**: 搭载官方 `apernet/hysteria/core/v2` Brutal 拥塞控制，按设定带宽（`-bandwidth`，默认 `50` Mbps，可设 `100`/`200` Mbps）恒定发包，抵消高延迟弱网的丢包降速。
  - **Native QUIC (`-proto quic`)**: 原生裸 UDP 上的 `quic-go` (TLS 1.3) 多路复用。
  - **Classic (`-proto classic`)**: 回退到 WebRTC DataChannel 帧多路复用（`bridge.go`）。
- **常驻守护与多客户端并发 (`v0.2.x`)**:
  - `exposeManager` 常驻信令，支持多客户端同时在线（`-max-peers 16`）与切网毫秒级原地重连。
  - 内置后台守护（`-daemon` / `-d`）、日志轮转（`-logfile`）、状态检查与优雅退出（`p2ptun status` / `p2ptun stop` / `-status-addr`）、真实 DNS 探测与 TCP 53 回退、网卡切换自愈及安全加固。

```
Client (Termux / Windows / Mac)         Server (WSL / Cloud Shell / VPS)
┌──────────────────────────────┐        ┌──────────────────────────────┐
│  127.0.0.1:2222 (or :1080)   │        │  Target TCP (:22 / :1080)    │
│  p2ptun connect (-daemon)    │        │  p2ptun expose (-daemon)     │
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

```sh
# 1. 服务端 (如 WSL / Linux 服务器，暴露 127.0.0.1:22，默认 Hysteria 2 恒定带宽)
# 注意: 默认不开启控制面板 HTTP 端口; 若需要 Web 监控可加 -status-addr 127.0.0.1:18222
P2PTUN_ID=wslssh-a1b2 P2PTUN_SECRET=hunter2 p2ptun expose -proto hy2 -bandwidth 100 -daemon

# 2. 客户端 (如 Termux / 笔记本) — 随后执行: ssh -p 2222 user@127.0.0.1
# 支持多台设备同时并发连接同一个服务端 (默认支持最多 16 台设备)
P2PTUN_PEER=wslssh-a1b2 P2PTUN_SECRET=hunter2 p2ptun connect -listen 127.0.0.1:2222 -proto hy2 -bandwidth 100 -daemon

# 3. 查看运行状态 / 活跃协议 / 原生 UDP 地址 / RTT 或停止守护进程
p2ptun status
p2ptun stop
```

## Security & Architecture Concepts

- **`ID` (`-id` / `-peer`)**：公共信令网（PeerJS）上的寻址标识（类似于电话号码）。客户端如果不指定 `-id`，会自动生成唯一的随机 ID（如 `phone-3b881fe7`），因此多台设备可并发呼叫同一个服务端 `-peer` 而不产生冲突。
- **`SECRET` (`-secret`)**：两端配对密钥与通行证。
  1. **信令防扫描**：信令协商时首先比对密钥，不匹配直接拒接（Reject），拒绝建立 WebRTC 连接。
  2. **传输加密认证**：在打洞成功的原生 UDP 上握手时，Hysteria 2 / QUIC 再次验证密钥，双重保障隧道安全。
- **控制面板（Control Panel）**：状态与控制 HTTP 接口（`/status`, `/healthz`, `/reconnect`, `/quit`）**默认严格关闭**，仅在显式传入 `-status-addr` / `-panel-addr` 时监听，避免任何多余端口暴露。

## Flags & Environment Variables

| Flag | Env | Description |
|---|---|---|
| `-id` | `P2PTUN_ID` | Peer ID on the signaling server |
| `-secret` | `P2PTUN_SECRET` | Pairing secret (must match on both sides) |
| `-peer` | `P2PTUN_PEER` | Remote expose peer ID (`connect` side) |
| `-target` | `P2PTUN_TARGET` | Exposed TCP endpoint (`expose`, default `127.0.0.1:22`) |
| `-listen` | `P2PTUN_LISTEN` | Local listen addr (`connect`, default `127.0.0.1:2222`) |
| `-proto` | `P2PTUN_PROTO` | Transport protocol: `hy2` (Hysteria 2 Brutal CC, default), `quic` (Native QUIC), `classic` |
| `-bandwidth` | `P2PTUN_BANDWIDTH` | Target bandwidth in Mbps for Hysteria 2 Brutal CC (default `50`) |
| `-broker` | `P2PTUN_BROKER` | Comma-separated signaling brokers (default `wss://0.peerjs.com,wss://1.peerjs.com`) |
| `-key` | `P2PTUN_KEY` | PeerJS API key (default `peerjs`) |
| `-stun` | `P2PTUN_STUN` | Comma-separated STUN servers |
| `-turn` | `P2PTUN_TURN` | Optional comma-separated TURN servers (`turn:user:pass@host:3478?transport=udp`) |
| `-force-relay` | `P2PTUN_FORCE_RELAY` | Force TURN relay transport for strict UDP-blocked networks |
| `-ice-ports` | `P2PTUN_ICE_PORTS` | Ephemeral UDP port range `MIN-MAX` for ICE candidates |
| `-nat1to1` | `P2PTUN_NAT1TO1` | External 1:1 NAT IP address(es) |
| `-proxy` | `P2PTUN_PROXY` | Explicit HTTP/HTTPS proxy URL for signaling WebSocket |
| `-keep-proxy` | `P2PTUN_KEEP_PROXY` | Keep system `HTTP_PROXY`/`HTTPS_PROXY` for signaling WebSocket |
| `-keepalive` | `P2PTUN_KEEPALIVE` | Keepalive interval (default `10s`) |
| `-max-peers` | `P2PTUN_MAX_PEERS` | Max concurrent connected clients on `expose` (default `16`) |
| `-on-demand` | `P2PTUN_ON_DEMAND` | Dial tunnel only when local TCP client connects (`connect`) |
| `-idle-timeout` | `P2PTUN_IDLE_TIMEOUT` | Hibernate idle on-demand tunnel after duration (default `10m`) |
| `-daemon`, `-d` | `P2PTUN_DAEMON` | Run in background daemon mode with auto-rotating logs |
| `-pidfile` | `P2PTUN_PIDFILE` | Custom PID file path |
| `-logfile` | `P2PTUN_LOGFILE` | Custom rotating log file path |
| `-log-max-mb` | `P2PTUN_LOG_MAX_MB` | Max log size in MB before rotation (default `10`) |
| `-status-addr` / `-panel-addr` | `P2PTUN_STATUS_ADDR` | Local HTTP status & control panel endpoint (optional, off by default unless specified) |
| — | `P2PTUN_DNS` | Custom DNS servers CSV |

## Build & Test

```sh
go test -v -race ./...
CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags '-s -w' -o p2ptun-arm64 .
```