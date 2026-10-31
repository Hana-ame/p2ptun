# 故障排查 (v0.2.1)

日志第一行会打印 `p2ptun 0.2.1 started (DNS: ...)`。也可随时通过 `p2ptun status` 查看当前守护进程状态、RTT、重连次数和最近错误。

## 1. 常驻运行与状态检查

### Termux 手机端常驻
```bash
# 启动后台守护进程 (自动写入 ~/.p2ptun/p2ptun-connect.log 并轮转)
P2PTUN_PEER=wslssh-a1b2 P2PTUN_SECRET=hunter2 p2ptun connect -daemon -status-addr 127.0.0.1:9222
termux-wake-lock

# 查看运行状态与实时 RTT
p2ptun status -status-addr 127.0.0.1:9222

# 优雅停止 (会自动向信令服发送 LEAVE 释放 ID)
p2ptun stop
```

或者使用仓库自带的 `scripts/termux-daemon.sh`：
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

v0.2.0 已内置三层自动容灾：
1. **多信令服自动切换**：默认依次尝试 `wss://0.peerjs.com` 与 `wss://1.peerjs.com`。
2. **真实 DNS 探测 + TCP 53 自动回退**：启动时自动探测最快 DNS；若校园网/企业网封锁 UDP 53，自动切换 TCP 53。
3. **系统代理自动回退**：若直连信令失败且环境变量原本配置了代理（如 `HTTPS_PROXY`），会自动回退通过代理连接信令。也可显式传 `-proxy http://127.0.0.1:7890`。

## 3. 多次重连或切换网络 (Wi-Fi <-> 5G)

v0.2.0 对多次连接做了彻底重构：
- **服务端信令常驻不重启**：`expose` 端信令长连接常驻，可同时接待多个客户端（`-max-peers 16`），收到重连 `OFFER` 时毫秒级原地应答，不再出现旧版 `busy pc=..., restarting session` 丢包等待 18 秒的问题。
- **网卡切换秒级自愈**：内置网卡 IP 监听，手机在 Wi-Fi 与移动数据间切换时立即触发重连（首次重连仅等待 400ms）。
- **DataChannel 心跳检测**：每 10 秒发送 `PING`/`PONG` 帧保活 NAT 映射并检测静默死连接。

## 4. 卡在 `pc state=connecting`（严格对称 NAT 或 UDP 被禁）

判断两端 UDP 是否通畅：
```bash
nc -u -w3 -z stun.cloudflare.com 3478 && echo UDP-OK
```

- 如果两端都是对称型 NAT（如部分移动运营商 4G/5G CGNAT）或企业防火墙完全封死 UDP：
  v0.2.0 新增了可选的 **TURN 中继支持**（仅在配置 `-turn` 时启用，未配置时保持 100% 纯 P2P）：
  ```bash
  # 两端同时加上 -turn 参数 (支持 udp / tcp / tls TURN):
  p2ptun expose  -id wslssh-a1b2 -secret hunter2 -turn "turn:user:pass@your.turn.server:3478?transport=udp"
  p2ptun connect -peer wslssh-a1b2 -secret hunter2 -turn "turn:user:pass@your.turn.server:3478?transport=udp"
  # 若网络完全禁止 UDP, 可加 -force-relay 与 ?transport=tcp
  ```
- 如果服务端所在公网机器做了端口映射或有固定公网 IP，可使用 `-ice-ports 50000-50100 -nat1to1 <公网IP>` 显著提升直连成功率。

## 5. `peer id already taken` (`ID-TAKEN`)

- 当进程被 `kill -9` 强杀或断电时，PeerJS 云服务器可能保留旧 ID 约 15~30 秒。
- v0.2.0 在正常停止（`Ctrl-C`、`SIGTERM`、`p2ptun stop`）时会自动关闭连接释放 ID；若遇异常断网后的 `ID-TAKEN`，`expose` 端会自动退避等待云端清理后重连，`connect` 端（未指定固定 `-id` 时）会自动轮换新随机 ID 立即连上。