# p2ptun

P2P TCP tunnel for remote SSH, built on **PeerJS cloud signaling + WebRTC DataChannel**.

- **Signaling**: PeerJS cloud (`wss://0.peerjs.com`) — only exchanges SDP/ICE, never sees payload.
- **Data plane**: WebRTC DataChannel, P2P (ICE + STUN). No relay, no TURN.
- Works from Termux (Android) to WSL / any Linux box behind NAT. Single static Go binary.

```
Termux / remote client          WSL / service owner
┌──────────────────┐            ┌───────────────────┐
│  127.0.0.1:2222  │  WebRTC    │  expose -> TCP 22 │
│  p2ptun connect  │◄──────────►│  p2ptun expose    │
└────────┬─────────┘  DataChannel└─────────┬─────────┘
         │  SDP/ICE only                    │
         └────────────► PeerJS cloud <──────┘
```

## Install

From [GitHub Releases](https://github.com/Hana-ame/p2ptun/releases)（手机国内直连不通时给 URL 加 `https://gh-proxy.com/` 前缀）:

```sh
# Termux / Android (arm64)
curl -L -o p2ptun https://github.com/Hana-ame/p2ptun/releases/latest/download/p2ptun_linux_arm64
chmod +x p2ptun && mv p2ptun $PREFIX/bin/

# Linux x86_64
curl -L -o p2ptun https://github.com/Hana-ame/p2ptun/releases/latest/download/p2ptun_linux_amd64
chmod +x p2ptun && sudo mv p2ptun /usr/local/bin/

# Windows PowerShell (临时跑 connect)
curl.exe -L -O https://github.com/Hana-ame/p2ptun/releases/latest/download/p2ptun_windows_amd64.exe

# 校验（可选）
curl -L -O https://github.com/Hana-ame/p2ptun/releases/latest/download/SHA256SUMS && sha256sum -c SHA256SUMS
```

## Quick start: remote SSH (最常用)

**1. 服务端** —— 在 TCP 服务所在机器（如 WSL，sshd 在 `127.0.0.1:22`）运行：

```sh
p2ptun expose -id wslssh-a1b2 -secret hunter2
```

**2. 客户端** —— 在远端（如 Termux）运行：

```sh
p2ptun connect -peer wslssh-a1b2 -secret hunter2 -listen 127.0.0.1:2222
```

**3. 连接：**

```sh
ssh -p 2222 user@127.0.0.1
```

连上后日志会打印 `tunnel established`；两端断线后都自动重连（固定 3s 重试），expose 端常驻等下一个 connect。

## Commands

### `p2ptun expose` — 暴露本机 TCP 服务（在服务所在机器跑）

```sh
p2ptun expose -secret SECRET [-id MYID] [-target HOST:PORT] [-stun csv]
```

- `-target` / `P2PTUN_TARGET`：要暴露的 TCP 端点，默认 `127.0.0.1:22`(sshd)。任何 TCP 服务都行：
  ```sh
  p2ptun expose -secret hunter2 -target 127.0.0.1:5173   # vite dev server
  p2ptun expose -secret hunter2 -target 127.0.0.1:8080   # 任一 HTTP 服务
  ```
- `-id` 省略时自动生成 `wslssh-<rand4>`，启动日志里能看到，把它填到客户端的 `-peer`。
- 一次服务一条 DataChannel（单个 connect）；对端断开后 3s 自动回到等待状态。

### `p2ptun connect` — 接入远端服务（在另一侧机器跑）

```sh
p2ptun connect -peer REMOTE_ID -secret SECRET [-listen HOST:PORT] [-stun csv]
```

- `-listen` / `P2PTUN_LISTEN`：本地监听地址，默认 `127.0.0.1:2222`。连上后访问该端口即等于访问对端 `-target`。
- `-peer` 必填 = expose 端的 `-id`。
- 本地 id 省略时自动生成 `phone-<rand4>`。

## Flags / env (两端通用)

| flag | env | description |
|---|---|---|
| `-id` | `P2PTUN_ID` | 自己在信令服务器上的 peer id（省略则自动生成） |
| `-secret` | `P2PTUN_SECRET` | 配对密钥，两端必须一致 (**必填**) |
| `-peer` | `P2PTUN_PEER` | 对端 id（仅 connect 需要） |
| `-target` | `P2PTUN_TARGET` | 暴露的 TCP 端点（仅 expose），默认 `127.0.0.1:22` |
| `-listen` | `P2PTUN_LISTEN` | 本地监听地址（仅 connect），默认 `127.0.0.1:2222` |
| `-broker` | `P2PTUN_BROKER` | 信令服务器，默认 `wss://0.peerjs.com`（备用 `wss://1.peerjs.com`） |
| `-key` | `P2PTUN_KEY` | PeerJS api key，默认 `peerjs` |
| `-stun` | `P2PTUN_STUN` | STUN 服务器逗号分隔。默认 `stun:stun.cloudflare.com:3478,stun:stun.l.google.com:19302,stun:stun.miwifi.com:3478,stun:stun.qq.com:3478` |
| — | `P2PTUN_DNS` | Termux 安全 DNS（默认 `223.5.5.5,114.114.114.114,8.8.8.8,1.1.1.1`），一般不用动 |

## More examples

```sh
# 手机访问 WSL 上的 vite dev server (5173)
# WSL:
p2ptun expose -id myvite -secret s1 -target 127.0.0.1:5173
# 手机浏览器打开 http://127.0.0.1:5173
P2PTUN_PEER=myvite P2PTUN_SECRET=s1 p2ptun connect -listen 127.0.0.1:5173

# 反过来：WSL 访问手机 Termux 上的服务（方向对称，谁有服务谁 expose）
# Termux:
p2ptun expose -id phonehttp -secret s2 -target 127.0.0.1:8000
# WSL:
p2ptun connect -peer phonehttp -secret s2 -listen 127.0.0.1:8000

# 不想固定 id？两端都省略 -id/-peer 不行（connect 需要知道对端），
# 先跑一次 expose 从日志抄下自动生成的 id 再填进 connect 即可。
```

## Documentation

- [`docs/architecture.md`](docs/architecture.md) — signaling / frame protocol / backpressure / session lifecycle
- [`docs/troubleshooting.md`](docs/troubleshooting.md) — phone-side DNS/CA errors, `[::1]:53` signature, NAT limits

## Termux

```sh
pkg install openssh tmux -y
# transfer the linux-arm64 binary, then:
cp p2ptun-arm64 $PREFIX/bin/p2ptun && chmod +x $PREFIX/bin/p2ptun
P2PTUN_PEER=wslssh-a1b2 P2PTUN_SECRET=hunter2 p2ptun connect
termux-wake-lock   # keep phone awake
```

> Termux/Android 特有问题已内置修复，无需配置：Termux 没有
> `/etc/resolv.conf`，程序固定走公共 DNS（`P2PTUN_DNS` 可覆盖）；
> 证书会自动附加 `$PREFIX/etc/tls/cert.pem`。

## systemd (WSL / Linux)

See [`examples/p2ptun.service`](examples/p2ptun.service).

## How it works

1. Both peers connect a WebSocket to a PeerJS server and register an id/token (`OPEN`).
2. Connect side sends `OFFER` (SDP + secret in metadata); expose side verifies the
   secret, replies `ANSWER`; both exchange ICE `CANDIDATE`s through the server.
3. WebRTC DataChannel (ordered/reliable) carries a simple multiplexed frame
   protocol (`SYN`/`DATA`/`FIN` per TCP stream) with `bufferedAmount` backpressure.

## Releases

Published on GitHub: https://github.com/Hana-ame/p2ptun/releases

- `p2ptun_linux_amd64` / `p2ptun_linux_arm64` / `p2ptun_linux_armv7` — Linux (arm64 给 Termux)
- `p2ptun_windows_amd64.exe` — Windows 上临时跑 connect
- `SHA256SUMS`

GitHub Actions 的 tag→release 工作流已就绪（`.github/workflows/release.yml`，推 `v*` tag 自动构建）；
当前仓库 Actions 未启用，启用后即自动发布，否则按 `docs/troubleshooting.md` 手动流程发布。
手机国内直连 GitHub 不通时，给 release URL 加 `https://gh-proxy.com/` 前缀。

## Build

```sh
CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags '-s -w' -o p2ptun-arm64 .
```

## Notes / limits

- Pure P2P requires UDP on both networks (STUN srflx candidates). If either side
  filters UDP or sits behind a symmetric NAT, hole punching can fail; only a TURN
  relay can fix that (adds relay, excluded by design here).
- The pairing secret is the only gate between your peers; use SSH keys, not
  passwords, on the target.
