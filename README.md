# p2ptun (v0.5.2)

高性能、零中继费用、无公网 IP 限制的 P2P TCP 穿透隧道工具。

基于 **PeerJS SCTP 控制面调度 + 原生 ICE UDP 裸套接字直连打洞 (Hysteria 2 / QUIC)**，并内置**多租户并发常驻守护、网络自愈与企业级安全加固**。

---

## 目录

- [🌟 为什么选择 p2ptun？](#-为什么选择-p2ptun)
- [🚀 快速上手 (Quick Start)](#-快速上手-quick-start)
- [🎯 五大典型场景实战示例 (Examples)](#-五大典型场景实战示例-examples)
  - [示例 1：内网 Linux / WSL2 极速 SSH 穿透](#示例-1内网-linux--wsl2-极速-ssh-穿透)
  - [示例 2：自建安全高速网络代理 (HTTP / SOCKS5)](#示例-2自建安全高速网络代理-http--socks5)
  - [示例 3：Windows 远程桌面 (RDP) 与内部 Web 服务穿透](#示例-3windows-远程桌面-rdp-与内部-web-服务穿透)
  - [示例 4：多人 / 多设备同时并发连接同一个服务端 (Multi-Peer)](#示例-4多人--多设备同时并发连接同一个服务端-multi-peer)
  - [示例 5：安卓手机 (Termux) 常驻后台与 Wi-Fi/5G 切换自愈](#示例-5安卓手机-termux-常驻后台与-wifi5g-切换自愈)
- [⚙️ 参数与环境变量全景说明书 (Flags Reference)](#️-参数与环境变量全景说明书-flags-reference)
- [🛠️ 常驻守护与监控面板 (Daemon & Status)](#️-常驻守护与监控面板-daemon--status)
- [📡 协议选择与带宽调优指南 (hy2 Brutal CC)](#-协议选择与带宽调优指南-hy2-brutal-cc)
- [📖 技术架构与深度原理](#-技术架构与深度原理)

---

## 🌟 为什么选择 p2ptun？

| 方案 | 中继服务器成本 | 带宽上限 | 跨国/弱网高丢包表现 | 配置复杂度 |
|---|---|---|---|---|
| **传统中继 (FRP / ngrok)** | 必须自建公网 VPS | 受限于 VPS 昂贵的小带宽 (通常 1~5Mbps) | 丢包严重受 TCP 阻塞影响 | 需配置服务器端与客户端 |
| **传统 WebRTC P2P** | 无需中继服务器 | 取决于两端物理带宽 | **SCTP 强制 1 秒 RTO，一旦丢包断崖式降速卡死** | 需配置信令 |
| **p2ptun (v0.5.2)** | **完全无需自建服务器** | **直接跑满两端实际物理带宽** | **搭载 Hysteria 2 Brutal CC 恒定发包，丢包绝不退避** | **零认知负荷，一行命令搞定** |

### 核心解耦架构：
1. **控制面 (Control Plane)**：通过公共 PeerJS 集群（`0.peerjs.com` 等）握手，并建立轻量 SCTP 控制通道（`p2ptun-control`）。**此信道完全不走业务数据**，只负责秘钥鉴权、参数协商与底层 UDP 候选地址分发。
2. **数据面 (Data Plane)**：通过 `pion/ice/v4` 底层打通公网两端**专属的原生裸 UDP 5-Tuple 物理通道**，直接在裸 UDP 上运行官方 **Hysteria 2 (Brutal CC)** 或 **QUIC (TLS 1.3)**。彻底剔除传统 WebRTC DataChannel 框架的加解密损耗与 SCTP 拥塞瓶颈！

```text
客户端 (Client)                                          服务端 (Server)
┌──────────────────────────────┐                         ┌──────────────────────────────┐
│  127.0.0.1:2222 (本地监听)    │                         │  目标 TCP 服务 (:22 / :1080) │
│  p2ptun connect (-daemon)    │                         │  p2ptun expose (-daemon)     │
└──────────────┬───────────────┘                         └──────────────┬───────────────┘
               │           [控制面] WebRTC DataChannel                  │
               │   ◄════════════ (SCTP 仅用于协商调度) ════════════►   │
               │                                                        │
               │           [数据面] 原生裸 UDP 物理直连通道              │
               │   ◄────────────────────────────────────────────────►   │
               │          Hysteria 2 (Brutal 恒定发包) / QUIC           │
               │                                                        │
               │                    交换 SDP / ICE                      │
               └──────────────────► PeerJS Cloud ◄──────────────────────┘
```

---

## 🚀 快速上手 (Quick Start)

### 1. 服务端 (暴露服务)
在需要被访问的电脑（如 WSL、家庭 Linux 服务器、局域网机器）上运行：
```bash
# 语法: p2ptun expose <隧道名称> -secret <配对秘钥> [其他参数]
p2ptun expose my-ssh -secret hunter2 -daemon
```

### 2. 客户端 (连接服务)
在你的远程设备（如笔记本、办公室电脑、Termux 手机）上运行：
```bash
# 语法: p2ptun connect <目标隧道名称> -secret <配对秘钥> -listen <本地监听地址> [其他参数]
p2ptun connect my-ssh -secret hunter2 -listen 127.0.0.1:2222 -daemon
```

连接成功后，终端会打印就绪提示，直接访问本地端口即可建立穿透连接：
```bash
ssh -p 2222 user@127.0.0.1
```

> 💡 **零认知负荷传参 (Zero Cognitive Load)**：
> 你无需记忆到底是 `-id` 还是 `-peer`！
> - 既可以直接作为命令后的**位置参数**：`p2ptun expose my-tunnel` / `p2ptun connect my-tunnel`
> - 也可以使用 `-id`、`-peer` 或 `-name`（两端完全通用互通）
> - 环境变量 `P2PTUN_NAME`、`P2PTUN_ID`、`P2PTUN_PEER` 也同样双向等价通用！

---

## 🎯 五大典型场景实战示例 (Examples)

### 示例 1：内网 Linux / WSL2 极速 SSH 穿透

#### 服务端 (WSL2 / Linux 服务器)：
暴露本地默认的 22 端口，设置 100Mbps 恒定发包带宽并后台运行：
```bash
p2ptun expose wsl-dev -secret MySecret123 -target 127.0.0.1:22 -bandwidth 100 -daemon
```

#### 客户端 (Mac / Windows / 笔记本)：
将远程隧道映射到本地 2222 端口：
```bash
p2ptun connect wsl-dev -secret MySecret123 -listen 127.0.0.1:2222 -bandwidth 100 -daemon
```

#### 配合 `~/.ssh/config` 极简登录：
在客户端电脑的 `~/.ssh/config` 中加入：
```sshconfig
Host wsl
    HostName 127.0.0.1
    Port 2222
    User ubuntu
    ServerAliveInterval 15
    ServerAliveCountMax 3
```
随后在任何终端只需输入 `ssh wsl` 即可直连！

---

### 示例 2：自建安全高速网络代理 (HTTP / SOCKS5)

#### 服务端：
假设本地运行了一个代理服务端（例如监听在 `127.0.0.1:1080`）：
```bash
# 暴露本地 1080 代理端口
p2ptun expose my-proxy -secret ProxyPass888 -target 127.0.0.1:1080 -proto hy2 -bandwidth 200 -daemon
```

#### 客户端 (外出笔记本或手机)：
```bash
# 本地监听 1080
p2ptun connect my-proxy -secret ProxyPass888 -listen 127.0.0.1:1080 -proto hy2 -bandwidth 200 -daemon
```

#### 验证代理连通性：
```bash
curl -x http://127.0.0.1:1080 -I https://www.google.com
```
浏览器安装 SwitchyOmega 等插件，将 HTTP/SOCKS5 代理地址填入 `127.0.0.1:1080`，即可通过远端服务器网络高速浏览。

---

### 示例 3：Windows 远程桌面 (RDP) 与内部 Web 服务穿透

#### 穿透 Windows RDP (3389)：
```bash
# 服务端 (Windows 机器，在 PowerShell 或 CMD 中执行)
p2ptun.exe expose office-pc -secret SecretRDP -target 127.0.0.1:3389 -daemon

# 客户端 (笔记本电脑)
p2ptun connect office-pc -secret SecretRDP -listen 127.0.0.1:13389 -daemon
```
打开 Windows 自带的“远程桌面连接” (mstsc)，计算机地址输入 `127.0.0.1:13389` 即可流畅连接。

#### 穿透内部 WebUI / 控制面板 (如 8080 端口)：
```bash
# 服务端
p2ptun expose internal-web -secret SecretWeb -target 127.0.0.1:8080 -daemon

# 客户端
p2ptun connect internal-web -secret SecretWeb -listen 127.0.0.1:8080 -daemon
```
浏览器直接打开 `http://127.0.0.1:8080` 即可访问远端私网 Web 界面。

---

### 示例 4：多人 / 多设备同时并发连接同一个服务端 (Multi-Peer)

`p2ptun` 服务端具备高并发多租户支持（默认支持最多 16 台设备并发接入）：
* **服务端永久在线**：只需启动一次 `p2ptun expose group-server -secret TeamKey`。
* **客户端零冲突机制**：每个客户端连接时无需关心客户端 ID，系统会自动生成唯一匿名标识（如 `phone-3b881fe7`、`laptop-8a9d12ef`）。
* **独立资源隔离**：Client A（家里电脑）与 Client B（手机）同时连入时，服务端分别为其协商独立的裸 UDP 5-tuple 与 Hysteria 2 会话，网络互不干扰，Client A 的弱网重传不会拖慢 Client B。

```bash
# 手机端
p2ptun connect group-server -secret TeamKey -listen 127.0.0.1:2222 -daemon

# 笔记本端 (同时连接)
p2ptun connect group-server -secret TeamKey -listen 127.0.0.1:2222 -daemon
```

---

### 示例 5：安卓手机 (Termux) 常驻后台与 Wi-Fi/5G 切换自愈

在安卓手机 Termux 环境下，网络切换非常频繁，且安卓后台容易杀死普通网络进程。`p2ptun` 针对移动端深度优化：

#### 一键使用内置管理脚本：
```bash
# 启动常驻后台 (自动获取安卓唤醒锁 wake-lock)
P2PTUN_NAME=my-ssh P2PTUN_SECRET=hunter2 bash scripts/termux-daemon.sh start

# 查看运行状态
bash scripts/termux-daemon.sh status

# 查看实时日志
bash scripts/termux-daemon.sh logs

# 安装为 Termux 开机自启 (需配合 Termux:Boot)
bash scripts/termux-daemon.sh install-boot
```

#### 秒级自愈特性：
- **网卡切换感知**：手机从 Wi-Fi 离开切换到 5G 移动蜂窝网络（或反之）时，底层探测模块毫秒级捕获 IP 变更，立即触发 400ms 平滑重连，无需手动重启。

---

## ⚙️ 参数与环境变量全景说明书 (Flags Reference)

命令行参数既可以通过 Flags 指定，也可以直接通过环境变量传递（两者等价）：

| 命令行 Flag | 对应环境变量 | 默认值 | 详细作用与最佳实践 |
|---|---|---|---|
| `<NAME>` / `-id` / `-peer` / `-name` | `P2PTUN_NAME` / `P2PTUN_ID` / `P2PTUN_PEER` | *(必填)* | **隧道标识名称**。`expose` 与 `connect` 两端通用互通，零记忆负担。 |
| `-secret` | `P2PTUN_SECRET` | *(必填)* | **配对密钥**。双重校验（信令防扫描拒绝 + UDP 传输加密认证）。 |
| `-target` | `P2PTUN_TARGET` | `127.0.0.1:22` | **服务端暴露的目标端口** (`expose` 专属)。可指向 SSH、HTTP 代理或内部服务。 |
| `-listen` | `P2PTUN_LISTEN` | `127.0.0.1:2222` | **客户端本地监听端口** (`connect` 专属)。供本地应用接入。 |
| `-proto` | `P2PTUN_PROTO` | `hy2` | **传输协议**：`hy2` (Hysteria 2 Brutal CC, 默认推荐)、`quic`、`classic`。 |
| `-bandwidth` | `P2PTUN_BANDWIDTH` | `50` | **Hysteria 2 Brutal CC 目标带宽** (Mbps)。推荐设为物理带宽上限 (如 50/100/200)。 |
| `-daemon`, `-d` | `P2PTUN_DAEMON` | `false` | **后台常驻守护模式**。脱离终端后台运行，输出重定向至轮转日志。 |
| `-pidfile` | `P2PTUN_PIDFILE` | `~/.p2ptun/*.pid` | 自定义守护进程 PID 文件存储路径。 |
| `-logfile` | `P2PTUN_LOGFILE` | `~/.p2ptun/*.log` | 自定义滚动日志文件路径。 |
| `-log-max-mb` | `P2PTUN_LOG_MAX_MB` | `10` | 日志单文件最大容量 (MB)，达到后自动滚动备份。 |
| `-status-addr` / `-panel-addr` | `P2PTUN_STATUS_ADDR` | *(默认关闭)* | **控制面板/HTTP 状态端点**。默认关闭保障零端口暴露；需查看时传 `127.0.0.1:18222`。 |
| `-broker` | `P2PTUN_BROKER` | `0.peerjs.com,...` | PeerJS 信令服务器列表，逗号分隔，断线自动轮询容灾。 |
| `-stun` | `P2PTUN_STUN` | Cloudflare / Google / 阿里 / 小米 | STUN 服务器列表，负责发现公网反射映射。 |
| `-turn` | `P2PTUN_TURN` | *(空)* | 可选 TURN 中继服务器 (`turn:user:pass@host:3478?transport=udp`)。 |
| `-force-relay` | `P2PTUN_FORCE_RELAY` | `false` | 强制使用 TURN 中继（仅在企业极端对称 NAT 或 UDP 完全被封时开启）。 |
| `-max-peers` | `P2PTUN_MAX_PEERS` | `16` | 服务端允许同时接入的最大并发客户端数量。 |
| `-on-demand` | `P2PTUN_ON_DEMAND` | `false` | 按需连接模式（仅当本地 TCP 收到连接时才唤醒 P2P 隧道）。 |
| `-idle-timeout` | `P2PTUN_IDLE_TIMEOUT` | `10m` | 按需模式下空闲超时自动休眠时间。 |

---

## 🛠️ 常驻守护与监控面板 (Daemon & Status)

### 1. 状态查询 (`p2ptun status`)
任何时候都可以通过 `p2ptun status` 查看后台守护进程的实时工作状态：
```bash
$ p2ptun status
p2ptun is running (PID 33458, mode: expose)
Tunnel ID:        my-tunnel
Target Endpoint:  127.0.0.1:1080
Transport Proto:  hy2 (Hysteria 2 Brutal CC)
Direct UDP Conn:  198.51.100.24:1570
Signaling Server: wss://0.peerjs.com (CONNECTED)
Active Peers:     1 online
Uptime:           2h 36m 39s
Traffic TX/RX:    2.09 GB / 27.75 MB
```

### 2. 安全退出 (`p2ptun stop`)
```bash
$ p2ptun stop
Stopped p2ptun daemon (PID 33458)
```

### 3. 可选控制面板 (HTTP Control Panel)
为防止不必要的本地端口暴露，控制面板 **默认严格禁用**。若你需要 Web 监控或自动化健康检查，可显式指定端口开启：
```bash
p2ptun expose my-tunnel -secret 123 -status-addr 127.0.0.1:18222 -daemon
```
开启后支持标准 REST 接口：
- `GET http://127.0.0.1:18222/status`：获取 JSON 格式的完整运行状态、统计指标与打洞地址。
- `GET http://127.0.0.1:18222/healthz`：容器健康检查探针。
- `POST http://127.0.0.1:18222/reconnect`：同源保护的强制重连触发器。
- `POST http://127.0.0.1:18222/quit`：同源保护的平滑退出触发器。

---

## 📡 协议选择与带宽调优指南 (hy2 Brutal CC)

`p2ptun` 支持三种不同的传输引擎，可通过 `-proto` 自由指定：

### 1. `hy2` (Hysteria 2 - 默认强烈推荐)
- **底层原理**：在穿透成功的原生 UDP 套接字上跑 Hysteria 2 协议栈与专有的 Brutal 拥塞控制算法。
- **核心优势**：传统的 BBR / Cubic 遇到丢包会立刻缩减窗口降低发包速率；而 Brutal 拥塞控制**绝不退避**，以恒定速率（由 `-bandwidth` 参数设定）发包，配合高速选择性重传 (SACK)，特别适合国际长途链路、校园网、晚高峰丢包网络。
- **推荐配置**：
  - 50M 宽带：`-bandwidth 50`
  - 100M 宽带：`-bandwidth 100`
  - 千兆/局域网：`-bandwidth 200` 或更高

### 2. `quic` (Native QUIC)
- **底层原理**：在原生 UDP 上运行标准 `quic-go` (TLS 1.3 多路复用)。
- **核心优势**：遵循标准 QUIC 拥塞控制，在本地局域网或高质量专线网络下 CPU 占用更低。

### 3. `classic` (WebRTC SCTP 回退)
- **底层原理**：传统 WebRTC DataChannel 多路复用。
- **适用场景**：作为特殊排查测试，或极个别原生 UDP 打洞被完全阻断的受限网络。

---

## 📖 技术架构与深度原理

关于原生 ICE 打洞时序、四阶段握手流程、Brutal 数学模型与故障自愈细节，请参阅：
- [docs/architecture.md](docs/architecture.md)：完整技术架构、时序图与并发隔离原理
- [docs/troubleshooting.md](docs/troubleshooting.md)：常见网络故障与 NAT 穿透诊断手册
- [examples/](examples/)：systemd 服务模板、环境配置示例与 SSH 配置进阶

---

## 💻 源码编译与安装

确保本地已安装 Go 1.22+：

```bash
# 本地直接编译
git clone https://github.com/Hana-ame/p2ptun.git
cd p2ptun
go build -ldflags="-s -w" -o p2ptun .

# 运行完整单元测试套件
go test -v .

# 交叉编译 Linux ARM64 (用于树莓派/安卓)
GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o p2ptun-arm64 .

# 交叉编译 Windows 64位
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o p2ptun.exe .
```

---

## 📄 License

MIT License.
