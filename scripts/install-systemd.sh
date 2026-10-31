#!/usr/bin/env bash
# scripts/install-systemd.sh - 一键安装并启动 Linux / WSL systemd 常驻服务
# 用法:
#   sudo bash scripts/install-systemd.sh expose  <ID> <SECRET> [TARGET=127.0.0.1:22]
#   sudo bash scripts/install-systemd.sh connect <PEER_ID> <SECRET> [LISTEN=127.0.0.1:2222]
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
    echo "Please run as root (sudo bash scripts/install-systemd.sh ...)" >&2
    exit 1
fi

MODE="${1:-}"
ARG1="${2:-}"
SECRET="${3:-}"
ENDPOINT="${4:-}"

if [ -z "${MODE}" ] || [ -z "${ARG1}" ] || [ -z "${SECRET}" ]; then
    echo "Usage:" >&2
    echo "  sudo bash $0 expose  <MY_ID>   <SECRET> [127.0.0.1:22]" >&2
    echo "  sudo bash $0 connect <PEER_ID> <SECRET> [127.0.0.1:2222]" >&2
    exit 2
fi

if [ -x "./p2ptun" ]; then
    install -m 0755 ./p2ptun /usr/local/bin/p2ptun
fi

mkdir -p /etc/p2ptun
chmod 0700 /etc/p2ptun

if [ "${MODE}" = "expose" ]; then
    TARGET="${ENDPOINT:-127.0.0.1:22}"
    ENV_PATH="/etc/p2ptun/env"
    SVC_NAME="p2ptun"
    cat > "${ENV_PATH}" <<EOF
P2PTUN_ID=${ARG1}
P2PTUN_SECRET=${SECRET}
P2PTUN_TARGET=${TARGET}
P2PTUN_STATUS_ADDR=127.0.0.1:18223
EOF
    chmod 0600 "${ENV_PATH}"
    /usr/local/bin/p2ptun service systemd expose > "/etc/systemd/system/${SVC_NAME}.service"
elif [ "${MODE}" = "connect" ]; then
    LISTEN="${ENDPOINT:-127.0.0.1:2222}"
    ENV_PATH="/etc/p2ptun/connect.env"
    SVC_NAME="p2ptun-connect"
    cat > "${ENV_PATH}" <<EOF
P2PTUN_PEER=${ARG1}
P2PTUN_SECRET=${SECRET}
P2PTUN_LISTEN=${LISTEN}
P2PTUN_STATUS_ADDR=127.0.0.1:18222
EOF
    chmod 0600 "${ENV_PATH}"
    /usr/local/bin/p2ptun service systemd connect > "/etc/systemd/system/${SVC_NAME}.service"
else
    echo "Unknown mode: ${MODE} (expected expose or connect)" >&2
    exit 2
fi

systemctl daemon-reload
systemctl enable --now "${SVC_NAME}"
systemctl status "${SVC_NAME}" --no-pager
