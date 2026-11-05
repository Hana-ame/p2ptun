# p2ptun (v0.4.0)

P2P TCP tunnel for remote SSH, proxies, and TCP services, built on **PeerJS signaling + WebRTC DataChannel** with **Hysteria 2 (Brutal CC)** transport acceleration.

- **Signaling**: PeerJS cloud (`wss://0.peerjs.com,wss://1.peerjs.com`) or self-hosted PeerJS server — exchanges SDP/ICE only.
- **Data plane**: WebRTC DataChannel, pure P2P (ICE + STUN) by default, with optional TURN relay support (`-turn`) for strict symmetric NAT / UDP-blocked networks.
- **Hysteria 2 & Brutal Congestion Control (`v0.4.0`)**:
  - **抗跨洋丢包加速**: 基于 WebRTC DataChannel 承载 Hysteria 2 (`github.com/apernet/hysteria/core/v2`)，默认启用 Brutal 拥塞控制算法。针对跨境长延迟（RTT ~180ms+）伴随丢包的网络环境，彻底解决传统 Cubic/BBR 遇到丢包窗口断崖式腰斩的问题，按预设速率（`-bandwidth`，默认 50Mbps，支持 100/200Mbps）恒定发包。
  - **16MB SCTP 接收窗口**: 将底层 Pion SCTP `MaxReceiveBufferSize` 由默认 1MB 提升至 16MB，彻底消除大 BDP（带宽延迟积）链路瓶颈；回环压测吞吐率突破 **112+ Mbps**。
  - **多协议灵活自适应**: 支持三模切换：`-proto hy2`（默认，Hysteria 2 Brutal CC）、`-proto quic`（标准 QUIC/Cubic）、`-proto classic`（轻量 9-字节自定义帧多路复用）。
  - **Web 预览适配**: HTTP 状态接口根路径 `/` 自动 307 重定向至 `/status`，完美兼容 Google Cloud Shell Web Preview 及常见反向代理。
- **Security & Stability Hardening (`v0.2.1`)**:
  - 配对密钥不再进入日志与进程列表（`-daemon` 子进程改由 `P2PTUN_SECRET` 传递）；`~/.p2ptun` 目录 0700、日志/pidfile 0600。
  - `/reconnect`、`/quit` 仅接受 POST + 同源校验（防 CSRF）；新增 `/quit` 优雅退出（Windows 无信号可用）。
  - `p2ptun stop` 校验 pidfile 中 PID 确为 p2ptun（防 PID 复用误杀），优先 `/quit`/SIGINT 并轮询最多 3s 才硬杀。
  - 单会话并发流上限 256 + 拨号可取消（防 frameSyn 洪泛耗尽 FD）；修复会话关闭死锁、背压阻塞控制帧、唤醒饥饿、首帧竞态、on-demand 假唤醒等并发问题。
- **Daemon & Multi-Session Ready (`v0.2.0`)**:
  - **常驻守护 (Daemonization)**: Built-in `-daemon` (`-d`), rotating logs (`-logfile`), PID management (`p2ptun status` / `p2ptun stop`), HTTP health endpoint (`-status-addr`), systemd & Termux boot generators (`p2ptun service`).
  - **多次连接 (Multi-Session & Instant Reconnect)**: Persistent signaling on `expose` decoupled from WebRTC sessions; supports concurrent multi-client connections (`-max-peers 16`) and zero-stall instant reconnection when clients restart or switch networks.
  - **多网络环境自适应 (Network Resilience)**: Real UDP/TCP 53 DNS probing & fallback, multi-broker failover, ambient proxy auto-fallback for signaling, DataChannel Ping/Pong keepalive + RTT tracking, local IP/interface change watcher (Wi-Fi ↔ 5G roaming), and optional `-on-demand` battery-saving mode.

```
Termux / remote clients (N)             WSL / Server (service owner)
┌──────────────────────────────┐        ┌──────────────────────────────┐
│  127.0.0.1:1080 (or :2222)   │ WebRTC │  exposeManager               │
│  p2ptun connect (-d)         │◄──────►│  p2ptun expose (-d)          │
│  [Hysteria 2 / Brutal CC]    │  DC    │  └─► TCP 127.0.0.1:1080/:22  │
└──────────────┬───────────────┘        └──────────────┬───────────────┘
               │         SDP/ICE only                  │
               └──────────────► PeerJS cloud ◄─────────┘
```

## Quick Start

### 场景 A：极速代理隧道（以 simple-http-proxy 为例）

```sh
# 1. 服务端 (如 Cloud Shell / 海外 VPS，运行代理服务于 1080)
p2ptun expose -id goproxy-tunnel -secret p2pproxy2026 -target 127.0.0.1:1080 -status-addr 0.0.0.0:8080 -bandwidth 100

# 2. 客户端 (本地电脑，如 Windows PowerShell / macOS / Termux)
p2ptun connect -peer goproxy-tunnel -secret p2pproxy2026 -listen 127.0.0.1:1080 -proto hy2 -bandwidth 100

# 3. 本地浏览器设置 HTTP/SOCKS5 代理为 127.0.0.1:1080 即可极速冲浪
```

### 场景 B：远程 SSH 穿透

```sh
# 1. 目标机 (如 WSL / Linux 服务器，sshd 监听 127.0.0.1:22)
P2PTUN_ID=wslssh-a1b2 P2PTUN_SECRET=hunter2 p2ptun expose -daemon

# 2. 客户端 (本地笔记本 / Termux)
P2PTUN_PEER=wslssh-a1b2 P2PTUN_SECRET=hunter2 p2ptun connect -listen 127.0.0.1:2222 -daemon

# 3. 连接
ssh -p 2222 user@127.0.0.1

# 4. 查看状态与停止
p2ptun status
p2ptun stop
```

## Flags & Environment Variables

| Flag | Env | Description |
|---|---|---|
| `-id` | `P2PTUN_ID` | Peer ID on the signaling server (`expose`) |
| `-secret` | `P2PTUN_SECRET` | Pairing secret (must match on both sides) |
| `-peer` | `P2PTUN_PEER` | Remote expose peer ID (`connect`) |
| `-target` | `P2PTUN_TARGET` | Exposed TCP endpoint (`expose`, default `127.0.0.1:22`) |
| `-listen` | `P2PTUN_LISTEN` | Local listen addr (`connect`, default `127.0.0.1:2222`) |
| `-proto` | `P2PTUN_PROTO` | Transport layer protocol: `hy2` (Hysteria 2 Brutal CC, default), `quic` (Standard QUIC/Cubic), `classic` (lightweight multiplexing) |
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
| `-keepalive` | `P2PTUN_KEEPALIVE` | DataChannel Ping/Pong keepalive interval (default `10s`) |
| `-max-peers` | `P2PTUN_MAX_PEERS` | Max concurrent connected clients on `expose` (default `16`) |
| `-on-demand` | `P2PTUN_ON_DEMAND` | Dial WebRTC tunnel only when local TCP client connects (`connect`) |
| `-idle-timeout` | `P2PTUN_IDLE_TIMEOUT` | Hibernate idle on-demand tunnel after duration (default `10m`) |
| `-daemon`, `-d` | `P2PTUN_DAEMON` | Run in background daemon mode with auto-rotating logs |
| `-pidfile` | `P2PTUN_PIDFILE` | Custom PID file path |
| `-logfile` | `P2PTUN_LOGFILE` | Custom rotating log file path |
| `-log-max-mb` | `P2PTUN_LOG_MAX_MB` | Max log size in MB before rotation (default `10`) |
| `-status-addr` | `P2PTUN_STATUS_ADDR` | Local HTTP status & healthcheck endpoint (e.g. `0.0.0.0:8080` or `127.0.0.1:18222`). 支持 `/status` 查看 JSON 详情，根路径 `/` 自动跳转 |
| — | `P2PTUN_DNS` | Custom DNS servers CSV (default auto-probes `/etc/resolv.conf` + `223.5.5.5,119.29.29.29,114.114.114.114,8.8.8.8,1.1.1.1`) |

## Daemon & Service Setup

### Termux (Android)

```sh
pkg install openssh tmux -y
cp p2ptun_linux_arm64 $PREFIX/bin/p2ptun && chmod +x $PREFIX/bin/p2ptun

# Option A: Built-in daemon mode
P2PTUN_PEER=wslssh-a1b2 P2PTUN_SECRET=hunter2 p2ptun connect -daemon
p2ptun status

# Option B: Helper script (supports start | stop | restart | status | logs | install-boot)
P2PTUN_PEER=wslssh-a1b2 P2PTUN_SECRET=hunter2 bash scripts/termux-daemon.sh start
bash scripts/termux-daemon.sh install-boot
```

### Linux / WSL (systemd)

```sh
# One-command systemd service installation:
sudo bash scripts/install-systemd.sh expose wslssh-a1b2 hunter2 127.0.0.1:22

# Or generate unit file directly:
p2ptun service systemd expose
```

## Build & Test

```sh
go test -v -race ./...
CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags '-s -w' -o p2ptun-arm64 .
```

## Documentation

- [`docs/architecture.md`](docs/architecture.md) — signaling, Hysteria 2 / QUIC / frame protocol, multi-peer manager, DNS/network resilience
- [`docs/troubleshooting.md`](docs/troubleshooting.md) — diagnostics, NAT/UDP checks, TURN configuration, daemon operations
