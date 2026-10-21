# p2ptun 架构 (v0.2.0)

设计目标：**远程 SSH/TCP 全程优先 P2P 直连；信令服务器（PeerJS cloud）只交换 SDP/ICE，
绝不经过业务数据。同时针对常驻守护、多次连接（并发多端与快速重连）、复杂网络环境提供完整的自愈与容灾能力。**

```
┌─ remote (Termux / Laptop) ─┐        ┌─ WSL / Service Owner ──────┐
│                            │        │                            │
│  ssh -p2222 127.0.0.1      │        │  sshd :22                  │
│        │                   │        │        ▲                   │
│  p2ptun connect (-daemon)  │        │  p2ptun expose (-daemon)   │
│        ▾                   │        │  (exposeManager: N peers)  │
│  WebRTC DataChannel ◄──────┼────────┼───────►│                   │
│  (P2P ICE/STUN + opt TURN) │        │        │                   │
└────────┬───────────────────┘        └────────┼───────────────────┘
         │     SDP / ICE / LEAVE only          │
         └──────────────► PeerJS cloud ◄───────┘
                   (0.peerjs.com / 1.peerjs.com)
```

## 1. 信令层（`peerjs.go`）

两端各开一条 WebSocket 连到 PeerJS broker（默认 `wss://0.peerjs.com,wss://1.peerjs.com` 自动容灾），
实现完整 PeerJS 协议交互：

| 消息 | 用途 |
|---|---|
| `OPEN` | 注册 peer id（token 随机生成，仅信令会话用） |
| `HEARTBEAT` + WS `Ping` | 每 5s 双向保活，配合 25s 滚动 `ReadDeadline` 秒级检测静默断网 |
| `OFFER` | connect 侧发起：本地 SDP + 唯一 `ConnectionID` + `metadata` 配对 secret |
| `ANSWER` | expose 侧校验 secret 后创建独立 `exposeSession` 并回复 SDP |
| `CANDIDATE` | 双向按 `ConnectionID` 路由转发 ICE candidate |
| `LEAVE` | 会话主动退出或重建时通知对端立即释放 `PeerConnection` |
| `ID-TAKEN` | 识别异常断线后的残留 ID 冲突：`expose` 自动退避等待清理，`connect` 自动轮换随机 ID |

## 2. 数据面（`bridge.go`：单 DataChannel 多路复用 + 应用层心跳）

一条 DataChannel 上多路复用任意数量的并发 TCP 连接：

```
9-字节帧头 + payload
┌──────┬────────────┬────────────┬─────────────┐
│ type │  connID(4) │ length(4)  │   payload   │
│ 1B   │             BE, uint32   │             │
└──────┴────────────┴────────────┴─────────────┘

type:
  1 = SYN  (新建 TCP 流，在启动读取协程前先行发送，确保首包绝不乱序丢失)
  2 = DATA (TCP 数据负载)
  3 = FIN  (优雅半关闭 EOF：排空已入队数据后再关闭本地 socket)
  4 = PING (通道保活探测，携带 8B 纳秒时间戳，维持 NAT 映射并测量 RTT，向后兼容 v0.1.x)
  5 = PONG (通道保活应答)
  6 = RST  (异常重置流，如目标端口连接失败或本地写入断开)
```

- **背压控制**：发送侧根据 `bufferedAmount` 做流控（>4MB 暂停读取 TCP，回落到 1MB 恢复）。
- **防饥饿隔离**：`PING`/`PONG` 控制帧在 `transport.dispatch` 直接处理；单条数据流队列满时带超时保护，绝不阻塞整条 DataChannel 上的其他 SSH 会话。
- **零协程泄漏**：`dcToConn` 同时监听 `st.ch`、`st.finCh` 与 `st.stopped`，任何异常断开都能立即回收协程。

## 3. 会话生命周期与多次连接管理（`main.go`）

- **`exposeManager`（服务端常驻与多客户端支持）**：
  - 信令 WebSocket 常驻保持在线，**不再因单个 WebRTC 会话结束而断开信令**。
  - 维护 `map[sessionKey]*exposeSession`（默认支持最多 16 个并发客户端同时在线）。
  - 当同一客户端重连发送新 `OFFER` 时，立即原地替换旧的非活跃会话并毫秒级回复 `ANSWER`（彻底消除旧版 `busy pc=..., restarting session` 丢弃 `OFFER` 导致的 18 秒卡顿）。
- **`connect`（客户端秒级重连与按需唤醒）**：
  - 每次建连生成独立 `ConnectionID`（`p2p-<hex>`），隔离旧会话延迟到达的 ICE Candidate。
  - `acceptLoop` 仅在 DataChannel 就绪（`<-trReady`）后才消费本地等待队列中的 TCP 连接，重连期间进入的 `ssh` 请求平滑等待隧道就绪而非直接重置报错。
  - 若连接曾成功建立后因切网断开，首次重连仅等待 `400ms`；连续失败则采用带随机抖动的指数退避（`1s -> 2s -> 4s -> 8s -> 15s`）。
  - 支持 `-on-demand` 按需打洞模式：平时零信令/零 WebRTC 消耗，当本地 `127.0.0.1:2222` 收到 TCP 连接时立即唤醒打洞，空闲超过 `-idle-timeout` 自动休眠。

## 4. 复杂网络自适应（`netfix.go`）

1. **真实 DNS 探测与 UDP/TCP 53 自动切换**：
   - 修复了 Go `net.Dial("udp", dns)` 不发包永远返回 `nil` 导致备用 DNS 与 TCP 53 永不触发的问题。
   - 启动及网络切换时并发发送真实 DNS 查询报文探测延迟，按响应速度排序 DNS 列表；若当前网络封锁 UDP 53，自动切换为 TCP 53 解析。
2. **环境代理智能回退**：
   - 启动时默认清理环境代理变量防止干扰 WebRTC UDP 打洞，但暂存原始代理地址。
   - 若直连 PeerJS 信令服务器失败（如受防火墙阻断）且启动前存在系统代理，自动回退通过系统代理建立信令 WebSocket（也可通过 `-proxy` 或 `-keep-proxy` 显式指定）。
3. **网卡切换监控（`watchNetworkChanges`）**：
   - 实时监测本机非回环 IP 变化（如手机 Wi-Fi 切 4G/5G、开关 VPN），检测到变化立即重排 DNS 并触发毫秒级重连。
4. **TURN 中继与端口定制**：
   - 默认纯 P2P（STUN），同时支持 `-turn turn:user:pass@host:port` 与 `-force-relay` 应对对称型 NAT 或纯 TCP/UDP 严格受限网络。

## 5. 守护常驻与运维接口（`daemon.go`）

- `-daemon` / `-d`：内置后台守护进程化，自动管理 PID 文件（`-pidfile`）与大小轮转日志（`-logfile`，默认 10MB 轮转）。
- `p2ptun status` / `p2ptun stop` / `p2ptun service [systemd|termux]`：一键查看实时状态（含 RTT、重连次数、累计流量、活跃流数）、优雅停止（自动发送 `LEAVE` 避免 `ID-TAKEN`）及生成系统服务配置。