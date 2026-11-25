# p2ptun

P2P TCP 隧道：把任何 TCP 服务（SSH / RDP / HTTP / 数据库）从一台没有公网 IP 的机器，映射到另一台机器的本地端口。

信令走 PeerJS（WebSocket），数据面走 **原生 ICE UDP 打洞直连**，不经任何中转服务器。

```
┌──────────────┐   ① WebSocket 信令（仅交换 SDP/ICE）    ┌──────────────┐
│  connect 端   │ ◄──────────── PeerJS Broker ─────────► │  expose 端    │
│ 你的笔记本    │                                        │ 内网/WSL/手机 │
│              │   ② ICE UDP 打洞后直连（数据全走这里）  │              │
│ 127.0.0.1:2222│ ═════════════════════════════════════► │ →127.0.0.1:22│
└──────────────┘                                        └──────────────┘
        ▲                                                        │
        └──────────── ssh -p 2222 127.0.0.1 ◄─────────────────────┘
```

信令成本可以忽略（只在建连时交换几十 KB），打洞成功后两端点对点传数据，所以**没有流量费用，也没有带宽上限**——上限只取决于你的 NAT 和网络质量。

---

## 目录

- [它解决什么问题](#它解决什么问题)
- [快速上手](#快速上手)
- [传输协议怎么选](#传输协议怎么选)
- [完整参数参考](#完整参数参考)
- [典型用法](#典型用法)
- [常驻运行](#常驻运行)
- [状态面板与监控](#状态面板与监控)
- [环境变量对照表](#环境变量对照表)
- [编译与测试](#编译与测试)
- [安全模型（以及什么没做）](#安全模型以及什么没做)
- [已知限制与故障排查](#已知限制与故障排查)

---

## 它解决什么问题

适用：

- **内网机器没有公网 IP**，但你想从外部 SSH / RDP 进去。两端都能上外网（哪怕在 NAT 后面）就行。
- **不想为中转付费，也不想自己架 relay**。打洞成功后数据不过第三方。
- **临时 / 个人用**：出差、换网络、设备在家、公司内网跳板。
- **Wi-Fi ↔ 4G 频繁切换的移动设备**（Termux），内置断线自动重连。

不适用（请直接选别的方案）：

- **需要 100% 连通的场景**。对称型 NAT（企业网、部分运营商 CGNAT）下 UDP 打洞会失败。有 `-turn` 可以兜底，但那就等于走了中转，带宽和费用又回到中转模型的账上。
- **需要强身份认证的多租户环境**。见 [安全模型](#安全模型以及什么没做)。
- **UDP 被完全封锁的网络**。打洞依赖 UDP 出站。

一句话：**它能用的场景非常好用，但它不能保证一定能用。** 先试，不通再换方案。

---

## 快速上手

两端都下载同一个二进制，各自执行一条命令。

### 1. 内网侧（expose：服务所在的那台机器）

```bash
./p2ptun expose mytunnel -secret 一个只有你知道的字符串 -target 127.0.0.1:22
```

- `mytunnel` 是隧道名，两端必须一致
- `-target` 是要暴露的本地 TCP 服务（这里是本机 SSH）
- 默认 `-proto hy2`

输出类似：

```
2024/01/01 12:00:00 p2ptun 0.5.2 starting (active DNS: 223.5.5.5, 119.29.29.29)
2024/01/01 12:00:00 expose: peer id = mytunnel-us1a2b3c4
2024/01/01 12:00:00 signaling: connected to wss://0.peerjs.com
```

记下它打印的实际 peer id。**如果隧道名可用**（没被别的进程占用），用名字就能连；如果名字已被抢占，程序会自动加后缀，此时需要用完整 peer id 做 connect。

### 2. 客户端侧（connect：你要从哪里连进去）

```bash
./p2ptun connect mytunnel -secret 一个只有你知道的字符串 -listen 127.0.0.1:2222
```

然后：

```bash
ssh -p 2222 127.0.0.1
```

`-listen` 只绑本机回环，外部访问不到，这是刻意的安全设计。

### 3. 确认走的是直连

connect 端日志里会有类似：

```
native udp: local candidate 192.168.1.5:41234 -> remote 203.0.113.7:51820
transport: hy2 ready (brutal 50 mbps)
```

看到 `hy2 ready` 或 `quic ready`，说明 UDP 直连已建立，数据没经过 broker。如果长时间停在 `connecting` 或最终报 `udp handshake timeout`，就是打洞失败了，见[故障排查](#已知限制与故障排查)。

---

## 传输协议怎么选

`-proto` 有四个可选值。默认 `hy2`，**绝大多数情况就用默认值**。

| 协议 | 数据面 | 拥塞控制 | 适用 |
|---|---|---|---|
| `hy2`（默认） | 原生 UDP + QUIC 握手（Hysteria 2） | Brutal（按固定速率发包，丢包不降速） | 弱网、跨国、丢包率高的线路 |
| `quic` | 原生 UDP + QUIC（quic-go） | 标准 NewReno / CUBIC | 网络干净、想要标准行为 |
| `classic` | WebRTC DataChannel 上做帧多路复用 | SCTP 自带 | **打洞成功但 UDP 直连被中间设备干扰**时的退路 |
| `dc-quic` | QUIC 跑在 DataChannel 之上 | quic-go 标准 | 实验性。想同时要 QUIC 的流控和 DataChannel 的穿透性 |

### 关于 `-bandwidth`（只在 `hy2` 下有意义）

Brutal 是**按你声明的速率**发包，不因丢包降速。所以：

- 填得比实际链路低 → 白白浪费带宽
- 填得比实际链路高 → 自己把自己打爆，丢包和重传反而更糟

```bash
# 家宽上行 30Mbps，就填 30 或略低
./p2ptun expose mytunnel -secret s -bandwidth 30
```

单位是 **Mbps**（不是 MB/s）。两端都应该填一致或接近的值。`quic` / `classic` / `dc-quic` 下这个参数被忽略。

### 切换协议

两端 `-proto` 必须一致，否则握手失败：

```bash
# 平时
./p2ptun expose t -secret s
./p2ptun connect t -secret s -listen 127.0.0.1:2222

# 打洞通了但数据传不动，退到 classic 试试
./p2ptun expose t -secret s -proto classic
./p2ptun connect t -secret s -listen 127.0.0.1:2222 -proto classic
```

---

## 完整参数参考

所有参数都有对应的环境变量（见[对照表](#环境变量对照表)），命令行优先级高于环境变量。

### 隧道标识

| 参数 | 默认 | 说明 |
|---|---|---|
| `-id` / `-peer` / `-name` | — | 隧道标识，三个名字完全等价。也可以直接作为位置参数传：`p2ptun expose mytunnel` |
| `-secret` | — | 配对密钥，两端必须一致。同时用于 UDP 层握手校验 |

```bash
# 这四种写法完全等价
./p2ptun expose mytunnel -secret s
./p2ptun expose -id mytunnel -secret s
./p2ptun expose -peer mytunnel -secret s
./p2ptun expose -name mytunnel -secret s
```

### 端点

| 参数 | 默认 | 说明 |
|---|---|---|
| `-target` | `127.0.0.1:22` | expose 端要暴露的 TCP 服务地址 |
| `-listen` | `127.0.0.1:2222` | connect 端本地监听地址 |

两个参数都会做格式校验，`host:port` 写错会直接报错退出，不会静默跑到奇怪的状态：

```bash
$ ./p2ptun expose t -secret s -target bad
invalid -target "bad": expected host:port, e.g. 127.0.0.1:22

$ ./p2ptun connect t -secret s -listen 127.0.0.1:99999
invalid -listen "127.0.0.1:99999": port must be 0-65535, got "99999"
```

`-listen 127.0.0.1:0` 表示让内核随机分配端口，程序会打印实际端口。

```bash
./p2ptun connect t -secret s -target 127.0.0.1:3389    # expose 端: 暴露 RDP
./p2ptun connect t -secret s -listen 0.0.0.0:8080      # ⚠️ 谨慎: 绑到所有网卡
```

> `-listen 0.0.0.0:PORT` 会把隧道暴露给同网段所有人。除非你清楚后果，否则保持回环地址。

### 传输

| 参数 | 默认 | 说明 |
|---|---|---|
| `-proto` | `hy2` | `hy2` / `quic` / `classic` / `dc-quic`。写错会报错并列出合法值 |
| `-bandwidth` | `50` | Brutal 目标带宽，单位 Mbps，仅 `hy2` 生效 |
| `-keepalive` | `10s` | 隧道内 PING 间隔，用于保活 NAT 映射和探测死连接 |
| `-max-peers` | `16` | expose 端允许的最大并发对端数 |

```bash
./p2ptun expose t -secret s -proto quic -keepalive 5s -max-peers 32
```

### 信令与 NAT 穿透

| 参数 | 默认 | 说明 |
|---|---|---|
| `-broker` | `wss://0.peerjs.com,wss://1.peerjs.com` | 信令服务器，逗号分隔，会依次尝试 |
| `-key` | `peerjs` | PeerJS api key，用公共 broker 时保持默认 |
| `-stun` | 内置列表 | STUN 服务器，逗号分隔 |
| `-turn` | — | TURN 中转，格式 `user:pass@host:port`，逗号分隔 |
| `-force-relay` | 关 | 强制走 TURN 中转，不做打洞 |
| `-ice-ports` | 随机 | ICE 用的本地 UDP 端口范围，如 `50000-50100` |
| `-nat1to1` | — | 告诉 ICE "我的公网 IP 是这个"，用于已知 1:1 NAT |

```bash
# 公司网络封了公共 STUN，换自己的
./p2ptun connect t -secret s -stun stun.example.com:3478

# 固定端口范围，方便在防火墙上开口
./p2ptun expose t -secret s -ice-ports 50000-50100

# 对称 NAT，打洞不通，用 TURN 兜底（会走中转）
./p2ptun connect t -secret s -turn user:pass@turn.example.com:3478
```

### 代理

| 参数 | 默认 | 说明 |
|---|---|---|
| `-proxy` | — | 给信令 WebSocket 用的 HTTP/HTTPS 代理 |
| `-keep-proxy` | 关 | 默认程序会清掉系统 `HTTP_PROXY` 以免影响数据面；打开这个保留 |

```bash
./p2ptun connect t -secret s -proxy http://127.0.0.1:7890
```

### 挂后台与日志

| 参数 | 默认 | 说明 |
|---|---|---|
| `-daemon` / `-d` | 关 | 后台运行 |
| `-pidfile` | 自动 | PID 文件路径 |
| `-logfile` | 自动 | 日志文件路径，会轮转 |
| `-status-addr` | 关 | 本地状态面板监听地址，不填则不启动 |
| `-panel-addr` | — | `-status-addr` 的别名 |

```bash
./p2ptun expose t -secret s -daemon -status-addr 127.0.0.1:18222
```

### 按需连接

只在有本地 TCP 连接进来时才去建立隧道，空闲超时后自动断开。适合"一天只用几次，不想常年挂着"的场景。

| 参数 | 默认 | 说明 |
|---|---|---|
| `-on-demand` | 关 | 开启按需模式 |
| `-idle-timeout` | `10m` | 空闲多久后断开 |

```bash
./p2ptun connect t -secret s -listen 127.0.0.1:2222 -on-demand -idle-timeout 5m
```

代价：第一次连进来时会多等几秒（要现打洞）。

---

## 典型用法

### SSH 进内网 Linux 机器

```bash
# 内网机器
./p2ptun expose ssh -secret long-random-string -target 127.0.0.1:22 -daemon

# 你的笔记本
./p2ptun connect ssh -secret long-random-string -listen 127.0.0.1:2222 -daemon
ssh -p 2222 user@127.0.0.1
```

### 远程桌面 Windows

```bash
# Windows 机器
p2ptun.exe expose rdp -secret s -target 127.0.0.1:3389

# 客户端
./p2ptun connect rdp -secret s -listen 127.0.0.1:13389
# 然后用 RDP 客户端连 127.0.0.1:13389
```

### 访问内网 Web 服务

```bash
./p2ptun expose web -secret s -target 127.0.0.1:3000
./p2ptun connect web -secret s -listen 127.0.0.1:8888
# 浏览器打开 http://127.0.0.1:8888
```

`classic` 协议下多流是帧级多路复用的，浏览器并发请求没问题。

### 多个客户端连同一个服务

expose 端只跑一个进程，`-max-peers` 控制上限（默认 16）：

```bash
# 服务端
./p2ptun expose shared -secret s -target 127.0.0.1:22 -max-peers 8

# 三个不同的人都用同一个隧道名和 secret
./p2ptun connect shared -secret s -listen 127.0.0.1:2222
```

每个 connect 端各自监听自己的本地端口，互不干扰。

### 手机（Termux）常驻

```bash
pkg install golang git
git clone https://github.com/Hana-ame/p2ptun && cd p2ptun
CGO_ENABLED=0 go build -o p2ptun .
./p2ptun connect mytunnel -secret s -listen 127.0.0.1:2222 -daemon
```

手机换网络（Wi-Fi → 4G）时隧道会断，程序会自动重连并重新打洞，不用手动干预。

---

## 常驻运行

### 后台守护

```bash
./p2ptun expose t -secret s -daemon
./p2ptun status              # 查看状态
./p2ptun stop                # 停止
```

`p2ptun status` 的真实输出（它会打印 pidfile 位置和最近 10 行日志）：

```
p2ptun (connect): pid=48213 (pidfile=/home/me/.p2ptun/connect.pid)
--- 最近运行日志 (Recent logs) ---
2024/01/01 12:00:00 p2ptun 0.5.2 starting (active DNS: 223.5.5.5)
2024/01/01 12:00:00 signaling: connected to wss://0.peerjs.com
2024/01/01 12:00:01 native udp: hole punch ok
2024/01/01 12:00:01 transport: hy2 ready (brutal 50 mbps)
```

它**不会**打印流量统计表——那些数字在[状态面板](#状态面板与监控)里。

```bash
./p2ptun status -mode expose     # 明确看 expose 那个
./p2ptun stop -mode expose
```

### systemd

程序能直接生成 unit 文件：

```bash
./p2ptun service systemd expose
```

生成内容如下（`p2ptun connect` 同理，只是把 `expose` 换掉）：

```ini
# /etc/systemd/system/p2ptun.service
[Unit]
Description=p2ptun expose daemon (P2P TCP tunnel server)
Wants=network-online.target
After=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
EnvironmentFile=/etc/p2ptun/env
ExecStart=/usr/local/bin/p2ptun expose
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=3
NoNewPrivileges=true
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
```

注意两点：

- `ExecStart` 写死为 `/usr/local/bin/p2ptun`，所以先把二进制放到那里（或者自己改这一行）。
- `EnvironmentFile=/etc/p2ptun/env` 是模板里固定的路径，**不会**自动创建。所有参数都要写进这个文件，因为 `ExecStart` 光秃秃的没有任何 flag：

```bash
sudo mkdir -p /etc/p2ptun
sudo tee /etc/p2ptun/env >/dev/null <<'EOF'
P2PTUN_SECRET=long-random-string
P2PTUN_TARGET=127.0.0.1:22
P2PTUN_PROTO=hy2
EOF
sudo chmod 600 /etc/p2ptun/env
```

这样 secret 只存在这个 600 权限的文件里，不进 unit、也不进 `ps aux`。

安装并启动：

```bash
./p2ptun service systemd expose | sudo tee /etc/systemd/system/p2ptun.service >/dev/null
sudo systemctl daemon-reload
sudo systemctl enable --now p2ptun
systemctl status p2ptun
```

`ExecReload` 支持热重载（`systemctl reload p2ptun`），它会发 `SIGHUP` 让进程重新读取配置并重建隧道，不用断开重启。

### Termux（Android）

Termux 没有 systemd。`service termux` 生成的是一份可直接执行的启动脚本，不是要追加到 `~/.bashrc` 的片段：

```bash
./p2ptun service termux connect > ~/p2ptun.sh
chmod +x ~/p2ptun.sh
```

生成内容：

```sh
#!/data/data/com.termux/files/usr/bin/sh
# Termux 常驻守护脚本 (可放入 ~/.termux/boot/p2ptun.sh 或直接执行)
command -v termux-wake-lock >/dev/null 2>&1 && termux-wake-lock

export P2PTUN_NAME="${P2PTUN_NAME:-${P2PTUN_PEER:-${P2PTUN_ID:-wslssh-a1b2}}}"
export P2PTUN_PEER="${P2PTUN_NAME}"
export P2PTUN_SECRET="${P2PTUN_SECRET:-hunter2}"
export P2PTUN_LISTEN="${P2PTUN_LISTEN:-127.0.0.1:2222}"
export P2PTUN_PROTO="${P2PTUN_PROTO:-hy2}"

exec p2ptun connect -daemon
```

脚本里的 `hunter2` 和隧道名都是**占位默认值**，第一次运行前必须用环境变量覆盖：

```bash
export P2PTUN_SECRET=long-random-string
export P2PTUN_NAME=mytunnel
~/p2ptun.sh
```

想开机自启，放到 Termux:Boot 的目录：

```bash
mkdir -p ~/.termux/boot
cp ~/p2ptun.sh ~/.termux/boot/p2ptun.sh
```

需要装 Termux:Boot 应用才会生效。脚本里的 `termux-wake-lock` 用来防止系统休眠时杀掉进程。

---

## 状态面板与监控

加 `-status-addr` 才会启动面板（默认关闭，避免无意中暴露）：

```bash
./p2ptun connect t -secret s -daemon -status-addr 127.0.0.1:18222
```

四个端点：

| 端点 | 方法 | 用途 |
|---|---|---|
| `/status` | GET | 完整状态 JSON |
| `/healthz` | GET | 健康检查，200 = 信令在线或有活跃对端，503 = 都不满足 |
| `/reconnect` | POST | 立即触发重连 |
| `/quit` | POST | 优雅退出 |

```bash
curl -s 127.0.0.1:18222/status | jq
```

返回字段：

```json
{
  "version": "0.5.2",
  "pid": 48213,
  "mode": "connect",
  "peer_id": "mytunnel-us1a2b3c4",
  "remote_peer": "mytunnel",
  "endpoint": "127.0.0.1:2222",
  "proto": "hy2",
  "udp_endpoint": "192.168.1.5:41234",
  "broker": "wss://0.peerjs.com",
  "uptime_sec": 9381,
  "uptime": "2h36m21s",
  "signaling_up": true,
  "active_peers": 1,
  "active_streams": 3,
  "reconnects": 0,
  "bytes_tx": 15240192,
  "bytes_rx": 8842130,
  "last_rtt_ms": 42,
  "dns_servers": "223.5.5.5,119.29.29.29"
}
```

`last_error` 字段只在出过错时出现。

配合 systemd 做健康检查：

```bash
curl -fsS 127.0.0.1:18222/healthz || systemctl restart p2ptun
```

`p2ptun status -status-addr 127.0.0.1:18222` 会直接拉 `/status` 打印，等价于上面的 curl。

---

## 环境变量对照表

每个参数都有环境变量，适合放进容器或 systemd 的 `EnvironmentFile`。

| 参数 | 环境变量 |
|---|---|
| `-id` / `-peer` / `-name` | `P2PTUN_ID` / `P2PTUN_PEER` / `P2PTUN_NAME` |
| `-secret` | `P2PTUN_SECRET` |
| `-target` | `P2PTUN_TARGET` |
| `-listen` | `P2PTUN_LISTEN` |
| `-proto` | `P2PTUN_PROTO` |
| `-bandwidth` | `P2PTUN_BANDWIDTH` |
| `-broker` | `P2PTUN_BROKER` |
| `-key` | `P2PTUN_KEY` |
| `-stun` | `P2PTUN_STUN` |
| `-turn` | `P2PTUN_TURN` |
| `-force-relay` | `P2PTUN_FORCE_RELAY` |
| `-ice-ports` | `P2PTUN_ICE_PORTS` |
| `-nat1to1` | `P2PTUN_NAT1TO1` |
| `-proxy` | `P2PTUN_PROXY` |
| `-keep-proxy` | `P2PTUN_KEEP_PROXY` |
| `-keepalive` | `P2PTUN_KEEPALIVE` |
| `-max-peers` | `P2PTUN_MAX_PEERS` |
| `-on-demand` | `P2PTUN_ON_DEMAND` |
| `-idle-timeout` | `P2PTUN_IDLE_TIMEOUT` |
| `-daemon` | `P2PTUN_DAEMON` |
| `-pidfile` | `P2PTUN_PIDFILE` |
| `-logfile` | `P2PTUN_LOGFILE` |
| `-status-addr` | `P2PTUN_STATUS_ADDR`（兼容 `P2PTUN_PANEL_ADDR` / `P2PTUN_CONTROL_ADDR`） |

示例：

```bash
export P2PTUN_SECRET=long-random-string
export P2PTUN_PROTO=hy2
./p2ptun expose mytunnel
./p2ptun connect mytunnel -listen 127.0.0.1:2222
```

---

## 编译与测试

需要 **Go 1.26 或更高**（`go.mod` 声明 `go 1.26.0`）。低版本 Go 遇到 `go 1.26.0` 会报错退出。开启 `GOTOOLCHAIN=auto`（Go 1.21+ 默认行为）可以自动下载匹配的工具链。

```bash
git clone https://github.com/Hana-ame/p2ptun
cd p2ptun
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o p2ptun .
```

`CGO_ENABLED=0` 产出静态二进制，能在老 glibc 和 Termux 上直接跑。

交叉编译：

```bash
GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build -o p2ptun_linux_arm64 .
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o p2ptun_windows_amd64.exe .
GOOS=linux   GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -o p2ptun_linux_armv7 .
```

跑测试：

```bash
go test -race ./...
```

CI 在每次 push 和 PR 上跑 `gofmt` + `go vet` + `go test -race` + 构建；打 `v*` tag 时走 release 流程产出四个平台的二进制和 `SHA256SUMS`。

测试覆盖：参数解析与校验、`-proto` 别名与严格校验、端点格式校验、classic 帧协议端到端（SYN/DATA/FIN/RST、多流并发、TCP 半关闭、目标不可达回 RST、流注册表回收）、原生 UDP 端到端、弱网性能基线。

---

## 安全模型（以及什么没做）

### 做了什么

- **`-secret` 双向校验**：两端 secret 不一致无法建连，同时用于 UDP 层握手，避免他人直接怼你的 UDP 端口。
- **数据面点对点加密**：QUIC / Hysteria 2 自带 TLS，`classic` 走 WebRTC DataChannel 的 DTLS。broker 看不到隧道里的内容。
- **监听默认绑回环**：`-listen` 默认 `127.0.0.1:2222`，同网段的人访问不到。
- **状态面板默认关闭**：不传 `-status-addr` 就完全不启动 HTTP 服务。
- **面板的写操作有 CSRF 防护**：`/reconnect` 和 `/quit` 只接受 POST 且会校验来源，跨站请求被拒。
- **端点参数校验**：`-target` / `-listen` 格式错误立即报错，不会带着畸形配置运行。

### 没做什么

这点要说清楚，别指望它提供超出设计的能力：

- **没有用户体系**。只有一把共享 secret，不能给不同人发不同凭据，也无法单独吊销某个人。要吊销就换 secret，所有人都得改。
- **secret 不参与身份识别**，只参与配对校验。通过校验的一律视为可信。
- **没有流量审计和访问日志**。只有字节计数（`bytes_tx` / `bytes_rx`），查不到"谁在什么时候连了什么"。
- **没有限流**。单个 connect 端可以开到 `-max-peers` 允许的数量，链路带宽靠 Brutal 的 `-bandwidth` 自我约束，那是个软上限。
- **不防内部横向**。expose 端 `-target` 指到哪，connect 端就能碰哪。如果 `-target` 指向 `127.0.0.1:22`，那拿到 secret 的人就有你机器的 SSH。

所以：**secret 要当密码对待**。足够长、不重复使用、不放公开仓库。如果要多用户、要审计、要能吊销，p2ptun 不是对的东西，请用 WireGuard + 正经的 VPN 网关或者 SSH 网关。

---

## 已知限制与故障排查

### 打洞失败（最常见）

日志停在 `connecting`，最后报 `udp handshake timeout` 或 `ice failed`。

原因：两端至少有一方在**对称型 NAT** 后面。家庭宽带单层 NAT 一般没问题，企业网、部分移动网络（尤其 4G/5G CGNAT）容易失败。

处理顺序：

1. 确认两端 UDP 出站没被封。`nc -u -v <对方公网IP> <端口>` 能发包不代表能通，但完全发不出去说明被封了。
2. 加 `-stun` 换一个能用的 STUN。
3. 已知自己在 1:1 NAT 后（云主机的弹性 IP），加 `-nat1to1 <你的公网IP>`。
4. 还不行就用 `-turn` 兜底。这时候数据走中转，带宽受中转服务器限制。

### 打洞成功但数据传不动 / 传得很慢

`hy2` 下大概率是 `-bandwidth` 填得不对。Brutal 不会自己降速，填高了就是把链路打爆：

```bash
# 先用保守值试试
./p2ptun expose t -secret s -bandwidth 10
```

如果 `hy2` 一直不对劲，换 `quic`：

```bash
./p2ptun expose t -secret s -proto quic
./p2ptun connect t -secret s -proto quic
```

`quic` 有标准的拥塞控制，会自己适应链路。

如果 UDP 层通了但连接反复断，退到 `classic`（数据走 DataChannel/SCTP，某些中间设备对纯 UDP 的干扰更少）：

```bash
./p2ptun expose t -secret s -proto classic
./p2ptun connect t -secret s -proto classic
```

### `-proto` 写错了

会直接报错并列出所有合法值，不会像以前那样静默退回 `hy2`：

```
$ ./p2ptun expose t -secret s -proto quik
invalid -proto "quik": expected one of classic, dc-quic, hy2, quic
```

### 隧道名被占用

多个进程用同一个隧道名时，后来的会自动加随机后缀。日志里会打印实际 peer id：

```
expose: peer id = mytunnel-us1a2b3c4
```

把它当作 `connect` 的第一个参数就能连上那个具体实例。**长期使用建议给每个隧道起独立的名字。**

### 换网络后连不上

`hy2` / `quic` 下程序会自己重连重打洞。如果发现没恢复：

```bash
curl -X POST 127.0.0.1:18222/reconnect
```

或者看 `p2ptun status` 确认进程还在、日志里有没有反复失败。

### Windows 上 `-listen` 绑不上

Windows 的端口保留策略（Hyper-V / WSL 会保留大段端口）可能导致绑定失败，报 `bind: An attempt was made to access a socket in a way forbidden`。换端口，或者：

```powershell
netsh int ipv4 show excludedportrange protocol=tcp
```

避开这些区间。

### 性能参考

本机回环（无丢包）下 `hy2` 能跑满千兆；跨公网的实际数字取决于 RTT、丢包率和两端带宽，没有普适的"能跑多快"。真实网络下 `hy2` + 正确的 `-bandwidth` 通常比 `quic` 在丢包环境里表现更好，这是 Brutal 的设计目的——代价是它会毫不客气地占满你声明的带宽。

---

## License

见 [LICENSE](LICENSE)。
