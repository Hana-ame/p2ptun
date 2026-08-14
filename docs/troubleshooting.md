# 故障排查

先分清报错是**旧版还是新版**：日志第一行会打印
`p2ptun <version> started (DNS: ...)`。旧版（v0.1.0）没有任何版本行。

## 1. 手机: `lookup ... on [::1]:53: connection refused`

```
connect: session ended: dial: dial tcp: lookup 0.peerjs.com on [::1]:53:
        read udp [::1]:4xxxx->[::1]:53: read: connection refused (retry in 3s)
```

**这是 v0.1.0 独有的报错**。Termux 没有 `/etc/resolv.conf`，Go 解析器就
去连 `[::1]:53`——本机根本没有 DNS 服务，秒拒，然后每 3 秒重试一次
（每次重试后面跟一行 `pc state=closed`，那是 teardown 的正常清理，别当成新错误）。

解法：升级到 **v0.1.1+**（固定公共 DNS + Termux CA）。

```bash
pkill -f p2ptun 2>/dev/null          # 杀掉旧进程, 替换文件不杀进程
curl -fLO https://github.com/Hana-ame/p2ptun/releases/download/v0.1.3/p2ptun_linux_arm64
# 国内不通: https://gh-proxy.com/https://github.com/Hana-ame/p2ptun/releases/download/v0.1.3/p2ptun_linux_arm64
mv p2ptun_linux_arm64 $PREFIX/bin/p2ptun && chmod +x $PREFIX/bin/p2ptun
p2ptun version        # 必须输出: p2ptun 0.1.x (≥0.1.1)
bash p2ptun-run.sh    # 全新终端重跑
```

升级后若仍 `[::1]:53`：说明跑的还是旧文件/旧进程——`which -a p2ptun`、
`ls -l $PREFIX/bin/p2ptun`（应约 9.5MB，太小是 curl 中途产物）。

## 2. 手机: `certificate signed by unknown authority`

v0.1.0 的另一个问题：Termux CA 在 `$PREFIX/etc/tls/cert.pem`，Go 默认找不到。
v0.1.1+ 已自动附加。升级即解。

## 3. 新版仍 `dial: ... connection refused (retry in 3s)`

新版报错会写**具体 DNS 服务器**（如 `on 223.5.5.5:53`）或直接连不上 broker。
可能是：

- 手机当前网络出不了境外 wss：把 broker 换成备用的
  `P2PTUN_BROKER=wss://1.peerjs.com` 试试（都连不通 → 换网络）。
- DNS 被污染：`export P2PTUN_DNS=223.5.5.5:53,114.114.114.114:53`。
- 信令通了但一直 `pc state=connecting` → 看第 5 条。

## 4. `pc state=closed`

不是错误。它是每次会话收尾 `pc.Close()` 的正常日志，几乎总是跟在
`session ended: ... (retry in 3s)` 之后同一瞬间出现。看问题看 session ended 那行。

## 5. 卡在 `pc state=connecting`（ICE 打洞失败）

| 症状 | 含义 |
|---|---|
| 两端都 `connected` | ✅ 通了 |
| 一直 `connecting`，几秒后 `failed` | 打洞失败 |

判断/处理：

- 服务端（WSL）的 `/etc/resolv.conf` 网络、以及两端 UDP 是否被过滤：
  ```bash
  # 两端各跑一次, 看 STUN(srflx) 是否能拿到公网 IP
  nc -u -w3 -z stun.cloudflare.com 3478 && echo UDP-OK   # WSL 上测试
  ```
- 手机用 Wi-Fi / 4G / 热点各试一次；真机网络通常能通，沙箱/公司网络常过滤 UDP。
- 对称 NAT 或无 UDP：**只有 TURN 能解，而 TURN 就是数据中继，违反本项目
  "不含转发" 的设计约束，不做**。

## 6. WSL 服务端排查

```bash
systemctl status p2ptun          # active?
journalctl -u p2ptun -n 50 --no-pager   # 看 expose 侧完整日志
```

正常情况：`signaling ready id=wslssh-..` 常驻，两侧连上时出现
`pc state=connected` + `data channel open`。它的日志里也会打印
connect 侧该用的命令（含当前 secret）。

## 7. 下载/传输

- 国内 GitHub 直连超时：给 release URL 加前缀 `https://gh-proxy.com/`
  （见第 1 条命令）。
- 或用 adb：`adb push p2ptun-arm64 /sdcard/Download/`，Termux 里
  `bash p2ptun-in-termux.sh /sdcard/Download/p2ptun-arm64`。
- Windows 机器上传：`C:\Users\lumin\p2ptun-arm64` 是仓库现成包
  （每次 Release 后同步）。