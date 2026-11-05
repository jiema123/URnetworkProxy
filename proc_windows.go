//go:build windows

package main

// 进程控制（Windows）：没有进程组和 SIGTERM，只能 TerminateProcess。

import (
	"os/exec"
	"syscall"
)

const createNewProcessGroup = 0x00000200

func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
}

func stopProc(cmd *exec.Cmd, hard bool) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// Windows 没有优雅退出信号，直接结束进程
	cmd.Process.Kill()
}

// Windows 上没有 pgrep，端口被占时交给系统报错，用户手动关掉即可
func killSquatters(bin string) bool { return false }
