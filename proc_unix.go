//go:build darwin || linux

package main

// 进程控制（类 Unix）：子进程放独立进程组，按组发信号，能连子进程一起收掉。

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// stopProc 先发 TERM，调用方决定后续是否 hard 补 KILL
func stopProc(cmd *exec.Cmd, hard bool) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if hard {
		sig = syscall.SIGKILL
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil {
		cmd.Process.Kill()
	}
}

// killSquatters 端口被旧进程占着时，按可执行文件路径把它清掉
func killSquatters(bin string) bool {
	out, err := exec.Command("pgrep", "-f", bin).Output()
	if err != nil {
		return false
	}
	killed := false
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil {
			syscall.Kill(pid, syscall.SIGTERM)
			killed = true
		}
	}
	time.Sleep(600 * time.Millisecond)
	return killed
}
