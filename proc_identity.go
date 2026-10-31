package main

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// processCommandLine 尽力取目标进程的命令行; 取不到时返回空串.
// Linux 读 /proc, Windows 用 tasklist, 其它平台用 ps.
func processCommandLine(pid int) string {
	pidStr := strconv.Itoa(pid)
	switch runtime.GOOS {
	case "linux":
		if b, err := os.ReadFile("/proc/" + pidStr + "/cmdline"); err == nil {
			return strings.ReplaceAll(string(b), "\x00", " ")
		}
	case "windows":
		out, err := exec.Command("tasklist", "/FI", "PID eq "+pidStr, "/FO", "CSV", "/NH").Output()
		if err == nil {
			return string(out)
		}
	default:
		out, err := exec.Command("ps", "-p", pidStr, "-o", "command=").Output()
		if err == nil {
			return string(out)
		}
	}
	return ""
}

// isP2ptunProcess 校验 pid 确实是 p2ptun (且模式大体匹配), 防止 pidfile
// 过期/被复用后 `p2ptun stop` 误杀无关进程. 无法确认身份时返回 false.
func isP2ptunProcess(pid int, mode string) bool {
	cmd := strings.ToLower(processCommandLine(pid))
	if cmd == "" || !strings.Contains(cmd, "p2ptun") {
		return false
	}
	if mode != "" {
		other := "expose"
		if mode == "expose" {
			other = "connect"
		}
		if strings.Contains(cmd, other) && !strings.Contains(cmd, mode) {
			return false
		}
	}
	return true
}
