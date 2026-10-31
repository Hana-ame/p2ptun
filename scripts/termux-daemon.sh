#!/usr/bin/env bash
# scripts/termux-daemon.sh - Termux / Linux 轻量常驻守护与管理脚本
# 支持命令: start | stop | restart | status | logs | install-boot
set -euo pipefail

MODE="${P2PTUN_MODE:-connect}"
PEER="${P2PTUN_PEER:-}"
SECRET="${P2PTUN_SECRET:-}"
ID="${P2PTUN_ID:-}"
LISTEN="${P2PTUN_LISTEN:-127.0.0.1:2222}"
TARGET="${P2PTUN_TARGET:-127.0.0.1:22}"
# 默认 18222: 避开 9222/9223 (Chromium/Edge CDP) 与 9229 (Node inspector)
STATUS_ADDR="${P2PTUN_STATUS_ADDR:-127.0.0.1:18222}"
STATE_DIR="${HOME}/.p2ptun"
ENV_FILE="${STATE_DIR}/config.env"
PID_FILE="${STATE_DIR}/p2ptun-${MODE}.pid"
LOG_FILE="${STATE_DIR}/p2ptun-${MODE}.log"

mkdir -p "${STATE_DIR}"
if [ -f "${ENV_FILE}" ]; then
    # set -a: 让 config.env 里不带 export 前缀的变量也导出给 p2ptun 子进程
    set -a
    # shellcheck disable=SC1090
    source "${ENV_FILE}"
    set +a
fi

BIN="$(command -v p2ptun || true)"
if [ -z "${BIN}" ] && [ -x "./p2ptun" ]; then
    BIN="./p2ptun"
fi

cmd="${1:-status}"
shift || true

case "${cmd}" in
    start)
        if command -v termux-wake-lock >/dev/null 2>&1; then
            termux-wake-lock || true
        fi
        if [ -z "${BIN}" ]; then
            echo "Error: p2ptun binary not found in PATH or current directory" >&2
            exit 1
        fi
        args=("${MODE}" "-daemon" "-pidfile" "${PID_FILE}" "-logfile" "${LOG_FILE}" "-status-addr" "${STATUS_ADDR}")
        [ -n "${SECRET}" ] && args+=("-secret" "${SECRET}")
        [ -n "${ID}" ] && args+=("-id" "${ID}")
        if [ "${MODE}" = "connect" ]; then
            [ -n "${PEER}" ] && args+=("-peer" "${PEER}")
            args+=("-listen" "${LISTEN}")
        else
            args+=("-target" "${TARGET}")
        fi
        "${BIN}" "${args[@]}" "$@"
        ;;
    stop)
        if [ -n "${BIN}" ]; then
            "${BIN}" stop -pidfile "${PID_FILE}" || true
        elif [ -f "${PID_FILE}" ]; then
            kill "$(cat "${PID_FILE}")" 2>/dev/null || true
            rm -f "${PID_FILE}"
        fi
        if command -v termux-wake-unlock >/dev/null 2>&1; then
            termux-wake-unlock || true
        fi
        ;;
    restart)
        "$0" stop || true
        sleep 1
        "$0" start "$@"
        ;;
    status)
        if [ -n "${BIN}" ]; then
            "${BIN}" status -mode "${MODE}" -pidfile "${PID_FILE}" -status-addr "${STATUS_ADDR}"
        elif [ -f "${PID_FILE}" ]; then
            echo "PID: $(cat "${PID_FILE}")"
            tail -n 15 "${LOG_FILE}" 2>/dev/null || true
        else
            echo "p2ptun is not running"
            exit 1
        fi
        ;;
    logs)
        tail -f "${LOG_FILE}"
        ;;
    install-boot)
        BOOT_DIR="${HOME}/.termux/boot"
        mkdir -p "${BOOT_DIR}"
        BOOT_SCRIPT="${BOOT_DIR}/start-p2ptun.sh"
        cat > "${BOOT_SCRIPT}" <<EOF
#!/data/data/com.termux/files/usr/bin/sh
termux-wake-lock
if [ -f "${ENV_FILE}" ]; then set -a; . "${ENV_FILE}"; set +a; fi
exec "${BIN:-p2ptun}" ${MODE} -daemon -pidfile "${PID_FILE}" -logfile "${LOG_FILE}" -status-addr "${STATUS_ADDR}"
EOF
        chmod +x "${BOOT_SCRIPT}"
        echo "Installed Termux boot script at ${BOOT_SCRIPT}"
        echo "Save your P2PTUN_PEER and P2PTUN_SECRET in ${ENV_FILE}"
        ;;
    *)
        echo "Usage: $0 {start|stop|restart|status|logs|install-boot} [extra p2ptun flags...]"
        exit 2
        ;;
esac
