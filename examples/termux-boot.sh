#!/data/data/com.termux/files/usr/bin/sh
# Termux 开机自启脚本示例 (examples/termux-boot.sh)
# 需要在 Termux 中安装 Termux:Boot APP:
#   mkdir -p ~/.termux/boot
#   cp examples/termux-boot.sh ~/.termux/boot/start-p2ptun.sh
#   chmod +x ~/.termux/boot/start-p2ptun.sh

# 获取后台唤醒锁, 防止安卓系统休眠时杀死进程
command -v termux-wake-lock >/dev/null 2>&1 && termux-wake-lock

# 配置参数
export P2PTUN_NAME="my-tunnel"
export P2PTUN_SECRET="your_strong_secret_2026"
export P2PTUN_LISTEN="127.0.0.1:2222"
export P2PTUN_PROTO="hy2"
export P2PTUN_BANDWIDTH="100"

# 启动后台守护进程
exec p2ptun connect -daemon
