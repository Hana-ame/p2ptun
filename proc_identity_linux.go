//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"
)

// processCommandLine 读 /proc/<pid>/cmdline. 参数之间以 NUL 分隔,
// 换成空格以便字符串匹配时能同时看到子命令 (expose/connect).
func processCommandLine(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(string(b), "\x00", " ")
}
