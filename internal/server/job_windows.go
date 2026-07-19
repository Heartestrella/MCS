package server

import (
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobObject ties child server processes to the panel's lifetime: when the
// panel process exits (including hot-swap restarts), Windows kills the whole
// job so no orphaned java.exe keeps holding world locks / ports.
var jobObject windows.Handle

func init() {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	jobObject = h
}

// assignToJob puts a started command's process into the panel job object.
func assignToJob(cmd *exec.Cmd) {
	if jobObject == 0 || cmd.Process == nil {
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return
	}
	windows.AssignProcessToJobObject(jobObject, h)
	windows.CloseHandle(h)
}

// hideWindow keeps child console processes from flashing windows. 面板本体
// 是控制台程序时子进程共用同一个窗口无所谓；GUI 版（windowsgui）没有
// 控制台，java/cmd/frpc 等子进程会各自弹一个黑窗，必须显式隐藏。
// 所有子进程的输入输出都走管道，隐藏控制台不影响功能。
func hideWindow(cmd *exec.Cmd) *exec.Cmd {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
	return cmd
}

// killTree force-kills a process and all its descendants. bat 启动的服务器进程
// 链是 cmd.exe → java.exe，只杀 cmd 会留下孤儿 java 继续锁住 mods 里的 jar。
func killTree(pid int) {
	hideWindow(exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))).Run()
}
