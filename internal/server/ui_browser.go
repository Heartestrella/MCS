//go:build webview && !windows

package server

// GUI 版(非 Windows):没有 WebView2,退而求其次——启动后自动用系统
// 默认浏览器打开面板,进程保持前台运行。日志同样落到 panel.log。

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func initUILogging() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(filepath.Dir(exe), "panel.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	log.SetOutput(f)
}

func uiHandlePortBusy(url string) bool {
	log.Printf("端口被占用，尝试连接已运行的面板: %s", url)
	openBrowser(url)
	return true
}

func runFrontend(url string) {
	openBrowser(url)
	select {}
}

func openBrowser(url string) {
	if runtime.GOOS == "darwin" {
		exec.Command("open", url).Start()
		return
	}
	exec.Command("xdg-open", url).Start()
}
