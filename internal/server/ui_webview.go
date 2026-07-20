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
	"unsafe"

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

// Win32 窗口改造 + 自绘标题栏支持。
// 隐藏系统标题栏用 Windows Terminal 同款做法：窗口样式原样保留
// （最小化动画 / Aero Snap 不受影响），子类化窗口过程后在
// WM_NCCALCSIZE 里把客户区顶边还原到窗口最顶端，系统标题栏连同
// 顶部残留的边框线一起消失。标题栏由前端画（web/index.html
// #guiBar），通过 Bind 的 _mcs* 函数控制窗口。
const (
	gwlpWndproc    = 0xFFFFFFFC // GWLP_WNDPROC(-4)，callee 按 32 位读取
	wmNCCalcSize   = 0x0083
	swpNoSize      = 0x0001
	swpNoMove      = 0x0002
	swpNoZOrder    = 0x0004
	swpFrameChange = 0x0020
	wmClose        = 0x0010
	wmNCLBtnDown   = 0x00A1
	htCaption      = 2
	htTop          = 12
	swMaximize     = 3
	swMinimize     = 6
	swRestore      = 9

	smCySizeFrame    = 33
	smCxPaddedBorder = 92
)

var (
	user32            = windows.NewLazySystemDLL("user32.dll")
	pSetWindowLong    = user32.NewProc("SetWindowLongPtrW")
	pCallWindowProc   = user32.NewProc("CallWindowProcW")
	pSetWindowPos     = user32.NewProc("SetWindowPos")
	pReleaseCapture   = user32.NewProc("ReleaseCapture")
	pSendMessage      = user32.NewProc("SendMessageW")
	pPostMessage      = user32.NewProc("PostMessageW")
	pIsZoomed         = user32.NewProc("IsZoomed")
	pShowWindow       = user32.NewProc("ShowWindow")
	pGetSystemMetrics = user32.NewProc("GetSystemMetrics")
)

type w32Rect struct{ Left, Top, Right, Bottom int32 }

func openWindow(url string) {
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		DataPath:  webviewDataDir(),
		WindowOptions: webview2.WindowOptions{
			Title:  "MCS Space — Minecraft 开服面板",
			Width:  1280,
			Height: 820,
			Center: true,
			IconId: 1, // rsrc_windows_*.syso 里的 RT_GROUP_ICON #1（web/logo.ico）
		},
	})
	if w == nil {
		// WebView2 运行时不可用：回落浏览器，面板保持后台运行
		fallbackBrowser(url)
		return
	}
	defer w.Destroy()

	hwnd := uintptr(w.Window())

	// 子类化：只拦 WM_NCCALCSIZE，其余照旧交给库的窗口过程
	var origProc uintptr
	frameless := windows.NewCallback(func(h, msg, wp, lp uintptr) uintptr {
		if msg == wmNCCalcSize && wp != 0 {
			// lp 指向 NCCALCSIZE_PARAMS，首个成员即建议客户区 RECT
			rc := (*w32Rect)(unsafe.Pointer(lp))
			top := rc.Top
			pCallWindowProc.Call(origProc, h, msg, wp, lp)
			rc.Top = top // 顶边不留给系统标题栏；左/右/下保留缩放边框
			if z, _, _ := pIsZoomed.Call(h); z != 0 {
				// 最大化时窗口会越出屏幕一个边框厚度，把顶边缩回来
				cy, _, _ := pGetSystemMetrics.Call(smCySizeFrame)
				pad, _, _ := pGetSystemMetrics.Call(smCxPaddedBorder)
				rc.Top += int32(cy + pad)
			}
			return 0
		}
		r, _, _ := pCallWindowProc.Call(origProc, h, msg, wp, lp)
		return r
	})
	origProc, _, _ = pSetWindowLong.Call(hwnd, gwlpWndproc, frameless)
	pSetWindowPos.Call(hwnd, 0, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoZOrder|swpFrameChange)

	// 页面加载前注入标记，前端据此显示自绘标题栏
	w.Init("window._mcsGui = true")
	_ = w.Bind("_mcsMin", func() { pShowWindow.Call(hwnd, swMinimize) })
	_ = w.Bind("_mcsMax", func() {
		if z, _, _ := pIsZoomed.Call(hwnd); z != 0 {
			pShowWindow.Call(hwnd, swRestore)
		} else {
			pShowWindow.Call(hwnd, swMaximize)
		}
	})
	_ = w.Bind("_mcsZoomed", func() bool {
		z, _, _ := pIsZoomed.Call(hwnd)
		return z != 0
	})
	_ = w.Bind("_mcsClose", func() { pPostMessage.Call(hwnd, wmClose, 0, 0) })
	_ = w.Bind("_mcsDrag", func() {
		// 交还鼠标捕获后伪造非客户区按下，让系统接管窗口拖动
		// （drag 期间 Aero Snap / 贴边分屏都可用）
		pReleaseCapture.Call()
		pSendMessage.Call(hwnd, wmNCLBtnDown, htCaption, 0)
	})
	_ = w.Bind("_mcsResizeTop", func() {
		// 客户区扩到窗口顶端后系统顶边缩放热区消失，前端顶部细条触发
		if z, _, _ := pIsZoomed.Call(hwnd); z != 0 {
			return
		}
		pReleaseCapture.Call()
		pSendMessage.Call(hwnd, wmNCLBtnDown, htTop, 0)
	})

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
