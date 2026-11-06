# p2ptun 核心原理与系统架构 (v0.5.1)

设计目标：**打造无需自建中继服务器、不受中心化带宽限制、无惧跨国/跨网高丢包恶劣链路的高性能 P2P TCP 穿透隧道。**

---

## 1. 设计背景与核心痛点（Why p2ptun?）

在远程运维（SSH）、远程代理（HTTP/SOCKS5）或内网穿透场景中，传统方案通常面临以下困境：

### 1.1 传统中心化中继（frp / nps / ngrok）的瓶颈
* **带宽成本与单点瓶颈**：所有业务流量必须经由一台公网服务器中转。公网云服务器的出口带宽极其昂贵，服务器带宽上限直接决定了穿透速率。
* **物理延迟翻倍**：若两台内网机器都在国内，而中继 VPS 部署在海外，流量需绕行大半个地球，网络延迟大幅飙升。

### 1.2 传统 WebRTC DataChannel 穿透方案的性能缺陷
WebRTC 能够实现 P2P 穿透打洞，但标准 WebRTC 的 **SCTP DataChannel** 在高吞吐或高延迟弱网下性能极差：
1. **DTLS + SCTP 双重开销**：用户态协议栈加密与封装层级过多，CPU 占用高，吞吐受限（通常只能跑到 15~30 Mbps）。
2. **SCTP 1 秒最小 RTO 陷阱**：标准 WebRTC SCTP 协议栈的重传超时（RTO）默认底限高达 1 秒。在轻微丢包或网络抖动时，TCP 连接会遭遇严重的队头阻塞（Head-of-Line Blocking），表现为 SSH 输入顿挫严重、代理下载瞬间跌零。
3. **拥塞控制断崖降速**：标准 SCTP/TCP 算法（如 Cubic）在遭遇丢包时会将拥塞窗口腰斩，导致跨网/跨国链路永远跑不满物理带宽。

### 1.3 p2ptun 的破局思路：调度面与数据面彻底解耦
`p2ptun` 创造性地提出了 **“WebRTC 仅作控制面调度，数据面完全交由原生裸 UDP + Hysteria 2”** 的架构：
* **调度面（Control Plane）**：利用全球公共 PeerJS 集群和轻量 WebRTC SCTP DataChannel 仅用于身份鉴权、协议协商与打洞凭证交换，**绝不通过任何业务数据**。
* **数据面（Data Plane）**：通过底层 `pion/ice.Agent` 独立打出**纯原生裸 UDP 直连通道**，在裸 UDP 套接字上直接运行现代化极速协议 **Hysteria 2（搭载 Brutal 恒定拥塞控制）** 或 **Native QUIC**。

---

## 2. 系统全景架构

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                            PeerJS Cloud Signaling                           │
│                 (wss://0.peerjs.com , wss://1.peerjs.com)                   │
└───────────────────────▲─────────────────────────────▲───────────────────────┘
                        │ 1. 注册与交换 SDP            │ 1. 发起呼叫 (带 secret)
                        │                              │
        ┌───────────────┴───────────────┐      ┌───────┴───────────────────────┐
        │  Server (WSL/Cloud Shell/VPS) │      │ Client (Termux/Laptop/Desktop)│
        │  p2ptun expose                │      │ p2ptun connect                │
        │  Target: 127.0.0.1:1080 (:22) │      │ Listen: 127.0.0.1:1080 (:2222)│
        └───────────────┬───────────────┘      └───────┬───────────────────────┘
                        │                              │
                        │ 2. WebRTC SCTP DataChannel   │
                        │    (label: p2ptun-control)   │
                        │ ◄══════════════════════════► │
                        │    • 配对密钥二次确认          │
                        │    • 协议/带宽协商 (hy2/quic) │
                        │    • 交换 Native ICE 候选地址 │
                        │                              │
                        │ 3. 独立底层 Native ICE 打洞   │
                        │ ◄──────────────────────────► │
                        │    pion/ice 原生 UDP 5-tuple │
                        │                              │
                        │ 4. 原生极速数据通道           │
                        │ ◄══════════════════════════► │
                        │    Hysteria 2 (Brutal CC)    │
                        │    (单连接多路复用海量 TCP 流)│
                        ▼                              ▼
                 [Target Service]                [Client Apps]
```

---

## 3. 端到端建连时序与穿透原理

完整建立一条 P2P 高速通道的生命周期分为四个严格递进的阶段：

```
Client (connect)                     Signaling (PeerJS)                  Server (expose)
      │                                      │                                  │
      │                                      │  1.1 Register ID (-id)           │
      │                                      │◄─────────────────────────────────│
      │                                      │  1.2 Signaling OPEN              │
      │                                      │─────────────────────────────────►│
      │                                      │                                  │
      │  2.1 Send OFFER (metadata=secret)    │                                  │
      │─────────────────────────────────────►│  2.2 Forward OFFER               │
      │                                      │─────────────────────────────────►│
      │                                      │      [Verify Secret in Meta]     │
      │                                      │      (Mismatch -> Reject & LEAVE)│
      │                                      │  2.3 Send ANSWER                 │
      │  2.4 Forward ANSWER                  │◄─────────────────────────────────│
      │◄─────────────────────────────────────│                                  │
      │                                      │                                  │
      │◄════════════════ 3. WebRTC SCTP DataChannel Open ══════════════════════►│
      │                  (label: "p2ptun-control")                              │
      │                                                                         │
      │  3.1 udpScheduleMsg{"init", proto:"hy2", bandwidth:100, ufrag, pwd}     │
      │────────────────────────────────────────────────────────────────────────►│
      │  3.2 udpScheduleMsg{"init", proto:"hy2", bandwidth:100, ufrag, pwd}     │
      │◄────────────────────────────────────────────────────────────────────────│
      │  3.3 Exchange ICE Candidates via Control Channel ("cand", "cand_end")   │
      │◄───────────────────────────────────────────────────────────────────────►│
      │                                                                         │
      │◄─────────── 4. pion/ice Direct Native UDP Punching (STUN) ─────────────►│
      │             (Hole punched! Local IP:Port <-> Remote IP:Port)            │
      │                                                                         │
      │◄═══════════ 5. Hysteria 2 / QUIC Handshake over Raw UDP ═══════════════►│
      │             (TLS 1.3 + Secret Auth + Brutal CC Rate-Limit)              │
      │                                                                         │
      │◄═══════════ 6. High-Throughput Bidirectional Data Streams ═════════════►│
```

### 阶段 1：公共信令发现与防扫描过滤（Signaling Phase）
1. **服务端挂牌**：服务端连入 PeerJS 公共集群（`wss://0.peerjs.com` 等），注册用户指定的隧道名称（如 `p2ptun expose goproxy-tunnel`，也支持 `-id` / `-peer` / `-name`）。
2. **客户端呼叫**：客户端发起连接（如 `p2ptun connect goproxy-tunnel`，也支持 `-id` / `-peer` / `-name`），系统自动生成本地独立客户端 ID（如 `phone-3b881fe7`），向信令服发送呼叫 `OFFER`，目标指向该隧道。
3. **信令防扫描校验**：客户端在 `OFFER` 的 `metadata` 中附带用户的 `-secret`。服务端收到后先校验密钥：
   * 若密钥不符，服务端**立即向信令服发送 `LEAVE` 并丢弃该连接**，绝对不触发 WebRTC 握手，使恶意扫描者连端口和协议特征都探测不到。
   * 若密钥正确，服务端创建独立会话 `exposeSession` 并回复 `ANSWER`。

### 阶段 2：SCTP 控制信道调度（Coordination Phase）
1. 双方经 PeerJS 完成基础 SDP 交换后，在底层的临时 WebRTC 链路上打开一条专用的内部控制通道：`p2ptun-control`（SCTP DataChannel）。
2. **专职控制调度**：该信道**完全不传输业务流量**，仅用于高可靠传输轻量控制指令（JSON 格式 `udpScheduleMsg`）：
   * 确认对端认证状态；
   * 协商传输协议（`hy2`、`quic` 或 `classic`）；
   * 协商下行目标带宽速率（`-bandwidth`，单位 Mbps）；
   * 生成全新的原生 ICE 证书：双方分别调用独立底层 `ice.Agent.GetLocalUserCredentials()`，通过此控制通道安全交换 `ufrag`（用户名）和 `pwd`（密码）。

### 阶段 3：独立 Native ICE 裸 UDP 打洞（Punching Phase）
1. 双方启动独立的 `pion/ice.Agent`，独立收集本地真实网卡物理 UDP 端口与 STUN 反射地址（`host`、`srflx`）。
2. 收集到的候选地址并不通过公共信令服务器发送，而是直接通过步骤 2 已建立的 `p2ptun-control` 专属加密信道交换。
3. 两端直接在公网上向彼此的 NAT 映射端口对向发送 STUN Binding Request 报文完成 UDP 打洞。
4. 打洞成功后，`agent.Accept()` / `agent.Dial()` 返回一条纯粹的 `ice.Conn`（原生 UDP 5-tuple 套接字），此时**完全脱离 WebRTC 协议栈与 DTLS/SCTP 的束缚**。

### 阶段 4：原生传输层握手与业务多路复用（Transport Phase）
1. 将原生打洞 UDP 套接字包装为标准的 `net.PacketConn`。
2. 在该套接字上直接启动 **Hysteria 2 服务端/客户端** 或 **Native QUIC 会话**。
3. 双方通过本地自签 ECDSA 证书快速完成 TLS 1.3 握手，并再次校验 `secret`。
4. 本地客户端所有发往监听端口（如 `127.0.0.1:1080`）的 TCP 连接，通过 Hysteria 2 / QUIC 的单 UDP 连接以流（Stream）的形式高速复用转发至目标端。

---

## 4. 数据面传输协议原理深度剖析

### 4.1 Hysteria 2 与 Brutal 拥塞控制原理（默认）
`p2ptun` 集成了官方核心 `github.com/apernet/hysteria/core/v2`，其最大特色是摒弃了传统丢包退避拥塞算法，引入 **Brutal 拥塞控制**：

* **传统 TCP（Cubic / Reno）与 BBR 的弱点**：
  * **丢包敏感**：TCP 将丢包视为网络发生严重拥塞的信号。一旦遇到跨国物理链路抖动、无线信号衰减导致 2%~5% 随机丢包，TCP 发送窗口会瞬间缩减 50% 甚至更多，传输速率暴跌。
  * **BBR 的 RTT 抖动退避**：BBR 虽然对丢包容忍度稍高，但当排队延迟升高或多流竞争时，依然会主动减速避让。
* **Brutal 拥塞控制的核心哲学**：
  * **固定速率注入（Fixed-Rate Ingestion）**：由用户明确声明自身可支配的带宽目标（例如 `-bandwidth 100` 即 100 Mbps）。
  * 算法以恒定速率持续向网络注入数据包：
    $$\text{Rate} = \frac{\text{Bandwidth (Mbps)} \times 1024 \times 1024}{8} \text{ Bytes/sec}$$
  * **快速选择性重传（SACK）**：即使网络链路发生 10%、20% 甚至 30% 的高丢包，Brutal 不做任何减速退避，而是立即通过高频 ACK 确认信息，在恒定发送队列中毫秒级补发丢失的数据包。
  * **实测结果**：在延迟 150ms~250ms、丢包率 5%~10% 的恶劣跨国跨网链路下，传统 TCP/SCTP 只能跑到 5~15 Mbps，而 Hysteria 2 Brutal CC 能稳定恒定跑满指定的 100 Mbps。

### 4.2 Native QUIC 原理（`-proto quic`）
* 基于 `github.com/quic-go/quic-go`，在原生裸打洞 UDP 上建立纯 QUIC (RFC 9000) 协议。
* **单连接多路复用（Multi-Stream Multiplexing）**：
  * 所有 TCP 连接全部映射为独立 QUIC 双向流（Bidirectional Stream）。
  * **消除队头阻塞**：一个流中的数据丢包重传，完全不影响同一 UDP 连接上的其他并发流读取。
* 适合网络条件较好、丢包率低但需要超大吞吐（局域网/同城千兆直连可跑满 300+ Mbps）的场景。

### 4.3 Classic 模式回退原理（`-proto classic`）
* 保留作为向后兼容或严格限制网络下的备用模式。
* 在单个 WebRTC SCTP DataChannel 之上，运行自研的 9 字节二进制帧多路复用器（`bridge.go`）：
  `[type: 1B] [connID: 4B BE] [length: 4B BE] [payload]`
* 内置应用层 PING/PONG 心跳测量、4MB 背压控制与半关闭（FIN）状态机。

---

## 5. 安全体系与双重鉴权机制原理

`p2ptun` 采用“公共信令公开展现，通信通道双重防线”的纵深防御模型：

```
                    Internet / Public Network
                               │
               [第一道防线: 信令层防扫描拦截]
               • metadata != secret ➔ 立即 Reject + LEAVE 断开
               • 恶意扫描探测者无法得知后端协议特征与服务状态
                               │
                               ▼
               [网络穿透: Native ICE UDP 打洞]
               • 经由专有控制信道动态交换临时 ufrag / pwd
               • 杜绝第三方公网嗅探注入伪造报文
                               │
                               ▼
               [第二道防线: 传输层身份认证与强加密]
               • TLS 1.3 ECDSA 端到端非对称加密
               • Hysteria 2 Authenticator (hy2Auth) 密钥核验
               • 校验失败即刻销毁 UDP 会话
                               │
                               ▼
                   [访问后端目标服务 (sshd / 代理)]
```

### 5.1 `ID` 与 `SECRET` 的本质区别
* **`ID`（公共门牌号 / 电话号码）**：
  * 仅用于公共信令服务器上的会话寻址。
  * 任何能连入该 PeerJS 集群的人都可以知道叫这个 `ID` 的节点在线，但**仅知道 `ID` 绝对无法建立隧道**。
* **`SECRET`（配对通信密码）**：
  * **必须两端保持绝对一致**，不对信令服务器公开。
  * **信令拦截**：第一道防线，密码不匹配连 WebRTC 握手都无法进行。
  * **传输校验**：第二道防线，原生 UDP 上通过 TLS 1.3 和 Hysteria 2 认证器二次严格验密。

---

## 6. 服务端多租户与并发架构原理（Multi-Peer Concurrency）

`p2ptun` 服务端（`expose`）为常驻长期运行而设计，完美支持多用户、多设备并发连入：

### 6.1 `exposeManager` 常驻状态机
* 服务端信令长连接**永久保持在线**，不随客户端的接入或断开而退出。
* 维护动态会话表 `sessions: map[string]*exposeSession`，默认支持多达 16 台设备同时接入（启动时可通过 `-max-peers 50` 调大）。

### 6.2 客户端零配置与动态匿名 ID 机制（零认知负荷）
* 隧道名称完全归一化：无论是 `expose` 还是 `connect`，用户都可以直接传参（`p2ptun connect goproxy-tunnel`）或使用 `-id` / `-peer` / `-name`，不再有区分 `-id` 还是 `-peer` 的心智负担。
* 客户端启动时系统自动为其生成全局唯一的随机 Client ID（格式如 `phone-3b881fe7`）。
* 因此，家庭电脑、办公笔记本、多部手机可以同时执行同一条简易命令（`p2ptun connect goproxy-tunnel -secret xxx`）并发连接同一个隧道，绝不发生信令碰撞。

### 6.3 独立的 UDP 5-Tuple 资源隔离
* 当 Client A（手机）与 Client B（笔记本）同时连入时：
  1. 服务端分别为 A 和 B 启动专属的 `pion/ice.Agent`；
  2. A 和 B 协商出完全不同的 UDP 5-tuple（例如 Server:59990 <-> ClientA:1570；Server:59992 <-> ClientB:8848）；
  3. 服务端为每个连接实例化独立的 Hysteria 2 服务端，彼此分配独立的 Brutal CC 速率。
* **互不干扰**：Client A 处于弱网丢包时，其 Brutal 快速补发完全不影响 Client B 的高速数据流动。

---

## 7. 复杂网络自愈、容灾与轻量运维原理

### 7.1 真实 DNS 延迟探测与 UDP/TCP 53 自动切换
* **传统缺陷**：Go 标库 `net.Dial("udp", "8.8.8.8:53")` 不发真实报文即可返回成功，导致部分屏蔽 UDP 53 的防火墙下程序假死。
* **p2ptun 机制**：启动时对内置 DNS 池（包括本地配置、阿里 DNS、腾讯 DNS、Google DNS、Cloudflare DNS）并发发送真实的 DNS 探测查询包，按实测 RTT 排序。若检测到当前网络封死 UDP 53，底层解析器**自动秒级无缝回退至 TCP 53 模式**。

### 7.2 动态网卡 IP 监听（Wi-Fi / 4G 毫秒级自愈）
* 后台协程（`watchNetworkChanges`）持续监测本机路由表与非回环 IP。
* 当手机从 Wi-Fi 离开切换到 4G/5G 蜂窝网络、或笔记本切换 VPN 时，探测到本机 IP 改变，客户端立即重置本地打洞环境并在 **400ms 内发起就地热重连**，无需用户干预。

### 7.3 控制面板（Control Panel）安全机制
* **默认严格关闭**：未显式传入 `-status-addr` / `-panel-addr` 时，**完全不开启任何 HTTP 监听端口**，避免产生多余的网络暴露面。
* **按需开启与指标监控**：指定端口时，内置轻量 HTTP 服务提供 `/status`（实时 JSON 监控数据，含当前吞吐量、在线人数、底层打洞 UDP 5-tuple、RTT、重连次数等）。
* **CSRF 免疫防御**：控制面板的敏感接口（如 `/quit` 退出进程、`/reconnect` 触发断线重连）强制校验 HTTP Method 为 `POST`，并严格核验 `Origin` 与 `Sec-Fetch-Site` 请求头，彻底防御来自恶意网页的跨站 CSRF 拒绝服务攻击。
