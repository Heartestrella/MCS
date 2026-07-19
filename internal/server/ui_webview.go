//go:build webview && windows

package server

// GUI 版前端层：-H windowsgui 编译，无控制台黑窗，面板界面嵌在
// WebView2 窗口里。关闭窗口 = 退出面板（job object 会连带结束所有
// 子服务器进程，和以前关掉黑窗的语义一致）。
//
// WebView2 运行时缺失（精简版 Win10 等）时回落系统默认浏览器打开，
// 面板继续在后台运行。

import (
	"log"
	"os"
	"path/filepath"

	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

// initUILogging: windowsgui 子系统没有 stdout，日志全部落到 exe 旁的
// panel.log，方便出问题时排查。
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

// uiHandlePortBusy: 端口被占基本等于「面板已经开着」——双击第二次
// 不该弹报错，直接开一个窗口连上已有实例。
func uiHandlePortBusy(url string) bool {
	log.Printf("端口被占用，尝试连接已运行的面板: %s", url)
	openWindow(url)
	return true
}

func runFrontend(url string) {
	openWindow(url)
}

func openWindow(url string) {
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		DataPath:  webviewDataDir(),
		WindowOptions: webview2.WindowOptions{
			Title:  "MCS Space — Minecraft 开服面板",
			Width:  1280,
			Height: 820,
			Center: true,
		},
	})
	if w == nil {
		// WebView2 运行时不可用：回落浏览器，面板保持后台运行
		fallbackBrowser(url)
		return
	}
	defer w.Destroy()
	w.Navigate(url)
	w.Run()
}

func fallbackBrowser(url string) {
	log.Printf("WebView2 运行时不可用，改用系统浏览器打开 %s", url)
	msg, _ := windows.UTF16PtrFromString(
		"未检测到 WebView2 运行时，已改用系统浏览器打开面板。\n\n" +
			"面板将在后台持续运行；想彻底退出请在任务管理器结束 mcs-panel-gui.exe，\n" +
			"或安装「Microsoft Edge WebView2 运行时」后重新打开以使用独立窗口。")
	title, _ := windows.UTF16PtrFromString("MCS Space")
	windows.MessageBox(0, msg, title, windows.MB_OK|windows.MB_ICONINFORMATION)

	u, _ := windows.UTF16PtrFromString(url)
	verb, _ := windows.UTF16PtrFromString("open")
	windows.ShellExecute(0, verb, u, nil, nil, windows.SW_SHOWNORMAL)
	select {} // 浏览器模式下窗口不归我们管，挂住进程让面板继续服务
}

// webviewDataDir: WebView2 的缓存/存储放进 data/ 里，保持「拷走整个
// 文件夹即完整迁移」的约定。
func webviewDataDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "data", "webview")
}
