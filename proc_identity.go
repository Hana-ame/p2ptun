package main

import (
	"strings"
)

// 本文件只保留与平台无关的判断逻辑; 取命令行的具体实现按平台分文件:
//   proc_identity_windows.go — Windows, 读 PEB 拿完整命令行
//   proc_identity_linux.go   — Linux, 读 /proc/<pid>/cmdline
//   proc_identity_unix.go    — 其它 unix (macOS/BSD), 用 ps

// isP2ptunProcess 校验 pid 确实是 p2ptun (且模式匹配), 防止 pidfile
// 过期/被复用后 `p2ptun stop` 误杀无关进程. 无法确认身份时返回 false.
func isP2ptunProcess(pid int, mode string) bool {
	return matchProcessCommandLine(processCommandLine(pid), mode)
}

// matchProcessCommandLine 是 isP2ptunProcess 的纯逻辑部分, 抽出来以便
// 不依赖真实进程就能测试 (CI 里造不出任意命令行形态的进程).
//
// 要点: mode 非空时, 命令行里**必须能证明**它跑的是该模式. 早期实现只
// 排除 "是另一种模式" 的情况, 于是像 tasklist 那种不含命令行的输出
// (只有 `"p2ptun.exe","1234",...`) 会同时被认成 expose 和 connect ——
// 正是 Windows 上 `stop -mode expose` 误杀 connect 进程的根因.
func matchProcessCommandLine(commandLine, mode string) bool {
	cmd := strings.ToLower(commandLine)
	if cmd == "" || !strings.Contains(cmd, "p2ptun") {
		return false
	}
	if mode == "" {
		return true
	}
	// 必须正面证明是这个模式, 而不是"看起来不像另一个模式".
	return strings.Contains(cmd, strings.ToLower(mode))
}
