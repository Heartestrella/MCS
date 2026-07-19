//go:build !webview

package server

// 纯 server 版前端层：控制台程序，日志走 stdout，启动后阻塞挂起。

func initUILogging() {}

// uiHandlePortBusy: server 版不接管端口冲突，维持原 log.Fatal 行为。
func uiHandlePortBusy(url string) bool { return false }

func runFrontend(url string) {
	select {}
}
