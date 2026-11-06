# 故障排查 (v0.5.1)

日志第一行会打印 `p2ptun 0.5.1 started (DNS: ...)`。也可随时通过 `p2ptun status` 查看当前守护进程状态、RTT、重连次数和最近错误。

## 1. 常驻运行与状态检查

### Termux 手机端常驻
```bash
# 1. 启动后台守护进程 (自动写入 ~/.p2ptun/p2ptun-connect.log 并轮转)
# 默认不开启任何控制面板 HTTP 端口; 若需要可通过 -status-addr / -panel-addr 显式指定
P2PTUN_PEER=wslssh-a1b2 P2PTUN_SECRET=hunter2 p2ptun connect -daemon -proto hy2 -bandwidth 100
termux-wake-lock

# 2. 查看运行状态与活跃传输协议 (无需 HTTP 端口, 直接读取 pidfile 探测)
p2ptun status

# 若启动时显式指定了 -status-addr 127.0.0.1:18222, 可通过 HTTP 状态接口查看:
# p2ptun status -status-addr 127.0.0.1:18222

# 3. 优雅停止 (会自动向信令服发送 LEAVE 释放 ID)
p2ptun stop
```

> **控制面板/状态接口端口说明**：
> `-status-addr`（别名 `-panel-addr` / `-control-addr`）**没有默认值，不指定监听地址即完全不开启**，保障零额外网络暴露。
> 若需要开启控制面板，推荐使用 `18222`（避开 Chromium/Edge CDP 调试端口 `9222/9223` 与 Node inspector `9229`）。

或者使用仓库自带的 `scripts/termux-daemon.sh`（默认已遵循不开启控制面板规范）：
```bash
P2PTUN_PEER=wslssh-a1b2 P2PTUN_SECRET=hunter2 bash scripts/termux-daemon.sh start
bash scripts/termux-daemon.sh status
bash scripts/termux-daemon.sh install-boot   # 安装 Termux 开机自启
```

### WSL / Linux 服务端常驻
```bash
sudo bash scripts/install-systemd.sh expose wslssh-a1b2 hunter2 127.0.0.1:22
systemctl status p2ptun
journalctl -u p2ptun -n 50 --no-pager
```

## 2. 信令连接不上 (`dial wss://0.peerjs.com ...`)

`v0.5.1` 内置三层自动容灾：
1. **多信令服自动切换**：默认依次尝试 `wss://0.peerjs.com` 与 `wss://1.peerjs.com`。
2. **真实 DNS 探测 + TCP 53 自动回退**：启动时自动探测最快 DNS；若校园网/企业网封锁 UDP 53，自动切换 TCP 53。
3. **系统代理自动回退**：若直连信令失败且环境变量原本配置了代理（如 `HTTPS_PROXY`），会自动回退通过代理连接信令。也可显式传 `-proxy http://127.0.0.1:7890`。

## 3. 多人连接与切网重连 (Wi-Fi <-> 5G)

- **多人同时在线（Multi-Peer）**：`expose` 端信令长连接常驻，默认支持最多 16 台设备并发连入（`-max-peers 16`）。所有客户端无需手动传 `-id`，使用相同 `-peer` 与 `-secret` 即可并发连接，各自独占原生 UDP 隧道。
- **网卡切换秒级自愈**：内置网卡 IP 监听，手机在 Wi-Fi 与移动数据间切换时立即触发重连（首次重连仅等待 400ms）。
- **心跳与保活**：底层 Hysteria 2 / QUIC 与控制通道双重保活，及时检测 NAT 映射断开。

## 4. 原生 UDP 打洞与传输排查（Hysteria 2 / QUIC）

`v0.5.1` 中 WebRTC SCTP 控制通道仅用于调度，数据在原生 UDP 上直连传输：
1. **确认 UDP 打洞是否成功**：
   查看服务端日志中是否出现：
   ```text
   expose: punch native UDP (proto=hy2, bandwidth=100 Mbps)...
   expose: native UDP punched! local=... remote=...
   expose: native Hysteria 2 server active (Brutal CC 100 Mbps)
   ```
2. **两端 UDP 连通性测试**：
   ```bash
   nc -u -w3 -z stun.cloudflare.com 3478 && echo UDP-OK
   ```
3. **严格对称 NAT 或 UDP 被彻底封锁**：
   - 可启用 TURN 中继支持（`-turn`）：
     ```bash
     p2ptun expose  -id wslssh-a1b2 -secret hunter2 -turn "turn:user:pass@your.turn.server:3478?transport=udp"
     p2ptun connect -peer wslssh-a1b2 -secret hunter2 -turn "turn:user:pass@your.turn.server:3478?transport=udp"
     # 若完全封禁 UDP, 可加 -force-relay 与 ?transport=tcp
     ```
   - 或回退到 Classic WebRTC DataChannel 模式：`-proto classic`。

## 5. 权限认证失败 (`bad secret`)

若日志中出现：
`expose: REJECT offer from phone-xxxx: bad secret`
说明客户端连接时提供的 `-secret` 与服务端的 `-secret` 不一致。服务端会在信令层直接拒接并断开，请核对两端的 `secret` 是否完全一致。

## 6. `peer id already taken` (`ID-TAKEN`)

- 当进程被强杀或异常掉电时，PeerJS 云服务器可能保留旧 ID 约 15~30 秒。
- `p2ptun` 正常停止（`Ctrl-C`、`SIGTERM`、`p2ptun stop`）时会自动关闭连接释放 ID；若遇异常断网后的 `ID-TAKEN`，`expose` 端会自动退避等待云端清理后重连，`connect` 端会自动轮换新随机 ID 立即连上。