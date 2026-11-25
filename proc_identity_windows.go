//go:build windows

package main

import (
	"os/exec"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processCommandLineWindows 取目标进程的完整命令行.
//
// tasklist 不够用: 它的 CSV 输出只有镜像名、PID、会话、序号和内存,
// 没有命令行. 旧实现直接拿 tasklist 的输出匹配 "expose"/"connect",
// 结果永远匹配不上, mode 参数在 Windows 上完全失效 (实测 p2ptun connect
// 进程对 "expose" 也返回 true, pidfile 复用时可能停错进程).
//
// 主路径直接读目标进程的 PEB: NtQueryInformationProcess 拿 PEB 地址,
// 再 ReadProcessMemory 逐层读出 ProcessParameters.CommandLine. 纯 Go,
// 不起子进程, 也不依赖 PowerShell (后者启动慢, 且受限环境下会被拦).
//
// 读不到时回退到 tasklist: 至少镜像名还在, 调用方仍能做基本的同名判断,
// 不至于因为读不了 PEB 就彻底认不出这是 p2ptun.
func processCommandLine(pid int) string {
	if cl, ok := commandLineFromPEB(uint32(pid)); ok && cl != "" {
		return cl
	}
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// commandLineFromPEB 通过 PEB 读取命令行. 失败返回 ok=false.
func commandLineFromPEB(pid uint32) (string, bool) {
	const (
		processQueryLimitedInformation = 0x1000
		processVMRead                  = 0x0010
	)

	h, err := windows.OpenProcess(processQueryLimitedInformation|processVMRead, false, pid)
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(h)

	var pbi processBasicInformation
	var retLen uint32
	if err := windows.NtQueryInformationProcess(
		h,
		windows.ProcessBasicInformation,
		unsafe.Pointer(&pbi),
		uint32(unsafe.Sizeof(pbi)),
		&retLen,
	); err != nil || pbi.PebBaseAddress == 0 {
		return "", false
	}

	var peb windows.PEB
	if !readProcessMemoryAt(h, pbi.PebBaseAddress, unsafe.Pointer(&peb), unsafe.Sizeof(peb)) {
		return "", false
	}
	if peb.ProcessParameters == nil {
		return "", false
	}

	var params windows.RTL_USER_PROCESS_PARAMETERS
	if !readProcessMemoryAt(h, uintptr(unsafe.Pointer(peb.ProcessParameters)),
		unsafe.Pointer(&params), unsafe.Sizeof(params)) {
		return "", false
	}

	cl := params.CommandLine
	if cl.Buffer == nil || cl.Length == 0 {
		return "", false
	}

	// Length 是字节数, 不含结尾的空字符.
	buf := make([]uint16, cl.Length/2)
	if !readProcessMemoryAt(h, uintptr(unsafe.Pointer(cl.Buffer)),
		unsafe.Pointer(&buf[0]), uintptr(cl.Length)) {
		return "", false
	}
	return windows.UTF16ToString(buf), true
}

// processBasicInformation 对应 PROCESS_BASIC_INFORMATION.
type processBasicInformation struct {
	ExitStatus                   uintptr
	PebBaseAddress               uintptr
	AffinityMask                 uintptr
	BasePriority                 int32
	UniqueProcessID              uintptr
	InheritedFromUniqueProcessID uintptr
}

// readProcessMemoryAt 是 ReadProcessMemory 的薄封装.
func readProcessMemoryAt(h windows.Handle, addr uintptr, dst unsafe.Pointer, size uintptr) bool {
	var read uintptr
	err := windows.ReadProcessMemory(h, addr, (*byte)(dst), size, &read)
	return err == nil && read == size
}
