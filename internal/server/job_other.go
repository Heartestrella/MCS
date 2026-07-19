//go:build !windows

package server

import (
	"os/exec"
	"syscall"
)

// hideWindow 在非 Windows 平台没有窗口可隐藏,但它是所有子进程的统一
// 准备入口,借此把子进程放进独立进程组,供 killTree 整组清理。
func hideWindow(cmd *exec.Cmd) *exec.Cmd {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	return cmd
}

// assignToJob: job object 是 Windows 概念;这里由进程组 + killTree 兜底。
func assignToJob(cmd *exec.Cmd) {}

// killTree kills the process group created in hideWindow, then the pid itself
// in case the child was started without Setpgid.
func killTree(pid int) {
	syscall.Kill(-pid, syscall.SIGKILL)
	syscall.Kill(pid, syscall.SIGKILL)
}
