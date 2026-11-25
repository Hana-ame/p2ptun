//go:build !linux && !windows

package main

import (
	"os/exec"
	"strconv"
)

// processCommandLine 在没有 /proc 的 unix (macOS / BSD) 上用 ps 取命令行.
func processCommandLine(pid int) string {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return ""
	}
	return string(out)
}
