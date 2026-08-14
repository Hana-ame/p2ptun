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

## Usage

```sh
# service owner (e.g. WSL, sshd on 127.0.0.1:22)
P2PTUN_ID=wslssh-a1b2 P2PTUN_SECRET=hunter2 p2ptun expose

# remote client (e.g. Termux) — then: ssh -p 2222 user@127.0.0.1
P2PTUN_PEER=wslssh-a1b2 P2PTUN_SECRET=hunter2 p2ptun connect -listen 127.0.0.1:2222
```

Flags / env:

| flag | env | description |
|---|---|---|
| `-id` | `P2PTUN_ID` | peer id on the signaling server |
| `-secret` | `P2PTUN_SECRET` | pairing secret, must match on both sides |
| `-peer` | `P2PTUN_PEER` | remote id (connect side) |
| `-target` | `P2PTUN_TARGET` | exposed TCP endpoint (expose), default `127.0.0.1:22` |
| `-listen` | `P2PTUN_LISTEN` | local listen addr (connect), default `127.0.0.1:2222` |
| `-broker` | `P2PTUN_BROKER` | signaling broker, default `wss://0.peerjs.com` (备用 `1.peerjs.com`) |
| `-key` | `P2PTUN_KEY` | PeerJS api key, default `peerjs` |
| `-stun` | `P2PTUN_STUN` | comma-separated STUN servers |
| — | `P2PTUN_DNS` | name servers for Termux-safe DNS (default `223.5.5.5,114.114.114.114,8.8.8.8,1.1.1.1`) |

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