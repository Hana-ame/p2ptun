package main

import "testing"

// TestMatchProcessCommandLine 锁住 isP2ptunProcess 的判断逻辑.
//
// 回归背景: Windows 上取命令行用的是 tasklist, 而它的 CSV 输出只有
// 镜像名/PID/会话/序号/内存, 没有命令行. 于是 "expose"/"connect" 永远
// 匹配不上, mode 参数形同虚设 —— 实测一个 `connect` 守护进程对
// mode="expose" 也返回 true, pidfile 复用时 `p2ptun stop -mode expose`
// 会把 connect 进程杀掉.
//
// 这里直接喂命令行字符串, 不依赖真实进程, 因此各平台都能跑.
func TestMatchProcessCommandLine(t *testing.T) {
	const (
		connectCmd = `C:\Users\me\.p2ptun\p2ptun.exe connect mytunnel -listen 127.0.0.1:2222 -daemon`
		exposeCmd  = `/usr/local/bin/p2ptun expose mytunnel -target 127.0.0.1:22 -daemon`
		// tasklist 的真实输出形态: 没有命令行, 只有镜像名和统计信息
		tasklistOutput = `"p2ptun.exe","16636","RDP-Tcp#19","1","20,452 K"`
	)

	tests := []struct {
		name        string
		commandLine string
		mode        string
		want        bool
	}{
		{"connect 命令行 + connect 模式", connectCmd, "connect", true},
		{"connect 命令行 + expose 模式", connectCmd, "expose", false},
		{"expose 命令行 + expose 模式", exposeCmd, "expose", true},
		{"expose 命令行 + connect 模式", exposeCmd, "connect", false},
		{"空 mode 不区分模式", connectCmd, "", true},
		{"空 mode 下 expose 也认", exposeCmd, "", true},
		{"非 p2ptun 进程一律拒绝", "/usr/bin/sshd -D", "", false},
		{"非 p2ptun 进程带 mode 也拒绝", "/usr/bin/sshd -D", "connect", false},
		{"空命令行拒绝", "", "connect", false},
		{"大小写不敏感", `/usr/local/bin/P2PTUN Connect t`, "connect", true},

		// 关键: 只有镜像名时无法证明它跑的是 expose, 必须拒绝.
		{"tasklist 输出不含命令行, 不能认成 expose", tasklistOutput, "expose", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchProcessCommandLine(tt.commandLine, tt.mode)
			if got != tt.want {
				t.Fatalf("matchProcessCommandLine(%q, %q) = %v, want %v",
					tt.commandLine, tt.mode, got, tt.want)
			}
		})
	}
}
