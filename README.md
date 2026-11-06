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

## 核心工作原理 (How It Works)

传统 P2P 穿透（如原生 WebRTC DataChannel）常受制于 SCTP 协议栈的 1 秒最小 RTO 与双重加解密开销，一旦遭遇跨网、跨国丢包，传输速率便会出现断崖式暴跌。

`p2ptun` 采用 **调度面与数据面彻底解耦** 的创新机制：

1. **信令发现与防扫描（Signaling Phase）**：
   通过公共 PeerJS 集群（`0.peerjs.com`）交换 WebRTC SDP。呼叫元数据中附带用户的 `SECRET`，服务端校验不匹配直接断开（Reject），完全杜绝未授权端口扫描。
2. **SCTP 控制信道调度（Coordination Phase）**：
   WebRTC 握手成功后仅建立一条专职控制信道（`p2ptun-control`），**完全不经过业务数据**，专职用于协议协商、目标带宽确认及交换底层独立的原生 ICE 打洞凭证。
3. **原生裸 UDP 打洞（Punching Phase）**：
   双方底层由独立的 `pion/ice.Agent` 发起对向 STUN 探测，在公网两端直接打通专属的 **原生裸 UDP 5-tuple 套接字**，彻底脱离 WebRTC 协议栈与 DTLS/SCTP。
4. **Hysteria 2 Brutal 拥塞控制（Transport Phase）**：
   在裸 UDP 套接字上直接运行官方 Hysteria 2 内核。无论网络延迟多高、丢包多严重，均按照设定的目标带宽（如 `-bandwidth 100`）**恒定发包并不做丢包退避**，配合极速选择性重传（SACK），彻底跑满物理带宽上限。
5. **多人并发连接（Multi-Peer）**：
   服务端 `exposeManager` 常驻运行。客户端无需手动指定 `-id`（自动随机分配），多台设备可并发呼叫同一个服务端 `-peer`，各自独立打洞并独占一条原生 UDP 隧道，互不干扰。

> 📖 详细系统架构、时序图、Brutal 数学模型与自愈原理请参阅 [docs/architecture.md](docs/architecture.md)。

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