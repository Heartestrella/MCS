package server

import (
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"mcs-panel/web"
)

// Run parses flags, wires up all HTTP routes and blocks serving the panel.
func Run() {
	initUILogging()
	port := flag.String("port", "8145", "panel listen port")
	httpsPort := flag.String("https-port", "8446", "HTTPS listen port (self-signed, for LAN voice rooms)")
	wtPort := flag.Int("wt-port", 4433, "WebTransport/QUIC UDP port for voice room screen relay (0 = off)")
	host := flag.String("host", "", "listen address (default 127.0.0.1; use 0.0.0.0 for LAN — enable panel password first!)")
	dataDir := flag.String("data", "", "data directory (default: ./data next to exe)")
	flag.Parse()
	httpsPortStr = *httpsPort

	if *dataDir == "" {
		exe, err := os.Executable()
		if err != nil {
			log.Fatal(err)
		}
		*dataDir = filepath.Join(filepath.Dir(exe), "data")
	}
	if err := os.MkdirAll(*dataDir, 0755); err != nil {
		log.Fatal(err)
	}

	mgr, err := NewManager(*dataDir)
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/instances", mgr.handleList)
	mux.HandleFunc("POST /api/instances", mgr.handleCreate)
	mux.HandleFunc("POST /api/instances/import", mgr.handleImport)
	mux.HandleFunc("POST /api/instances/generic", mgr.handleGenericCreate)
	mux.HandleFunc("PATCH /api/instances/{id}/generic", mgr.handleGenericUpdate)
	mux.HandleFunc("GET /api/instances/{id}", mgr.handleGet)
	mux.HandleFunc("DELETE /api/instances/{id}", mgr.handleDelete)
	mux.HandleFunc("PATCH /api/instances/{id}", mgr.handleUpdate)
	mux.HandleFunc("GET /api/instances/{id}/java", mgr.handleJavaInfo)
	mux.HandleFunc("GET /api/instances/{id}/addresses", mgr.handleAddresses)
	mux.HandleFunc("POST /api/instances/{id}/fixjava", mgr.handleFixJava)
	mux.HandleFunc("POST /api/instances/{id}/start", mgr.handleStart)
	mux.HandleFunc("POST /api/instances/{id}/stop", mgr.handleStop)
	mux.HandleFunc("POST /api/instances/{id}/restart", mgr.handleRestart)
	mux.HandleFunc("POST /api/instances/{id}/sleep", mgr.handleSleep)
	mux.HandleFunc("POST /api/instances/{id}/command", mgr.handleCommand)
	mux.HandleFunc("GET /api/instances/{id}/console", mgr.handleConsoleWS)
	mux.HandleFunc("GET /api/instances/{id}/props", mgr.handlePropsGet)
	mux.HandleFunc("POST /api/instances/{id}/props", mgr.handlePropsSet)
	mux.HandleFunc("GET /api/instances/{id}/players", mgr.handlePlayersGet)
	mux.HandleFunc("POST /api/instances/{id}/players", mgr.handlePlayersAction)
	mux.HandleFunc("GET /api/instances/{id}/stats", mgr.handlePlayerStats)
	mux.HandleFunc("GET /api/instances/{id}/health", mgr.handleHealth)
	mux.HandleFunc("GET /api/instances/{id}/chat", mgr.handleChatGet)
	mux.HandleFunc("GET /api/instances/{id}/logs", mgr.handleLogsList)
	mux.HandleFunc("GET /api/instances/{id}/logs/search", mgr.handleLogSearch)
	mux.HandleFunc("GET /api/instances/{id}/logs/{file}", mgr.handleLogGet)
	mux.HandleFunc("GET /api/instances/{id}/installed", mgr.handleInstalledList)
	mux.HandleFunc("POST /api/instances/{id}/installed", mgr.handleInstalledAction)
	mux.HandleFunc("POST /api/instances/{id}/installed/upload", mgr.handleInstalledUpload)
	mux.HandleFunc("GET /api/instances/{id}/files", mgr.handleFilesList)
	mux.HandleFunc("GET /api/instances/{id}/files/download", mgr.handleFileDownload)
	mux.HandleFunc("DELETE /api/instances/{id}/files", mgr.handleFileDelete)
	mux.HandleFunc("POST /api/instances/{id}/files/upload", mgr.handleFileUpload)
	mux.HandleFunc("GET /api/instances/{id}/files/content", mgr.handleFileRead)
	mux.HandleFunc("POST /api/instances/{id}/files/content", mgr.handleFileWrite)
	mux.HandleFunc("POST /api/instances/{id}/files/rename", mgr.handleFileRename)
	mux.HandleFunc("POST /api/instances/{id}/files/mkdir", mgr.handleFileMkdir)
	mux.HandleFunc("POST /api/instances/{id}/files/unzip", mgr.handleFileUnzip)
	mux.HandleFunc("GET /api/instances/{id}/files/zipdir", mgr.handleDirZip)
	mux.HandleFunc("GET /api/instances/{id}/core/check", mgr.handleCoreCheck)
	mux.HandleFunc("POST /api/instances/{id}/core/update", mgr.handleCoreUpdate)
	mux.HandleFunc("GET /api/instances/{id}/upgrade/versions", mgr.handleUpgradeVersions)
	mux.HandleFunc("POST /api/instances/{id}/upgrade", mgr.handleUpgrade)
	mux.HandleFunc("POST /api/instances/{id}/upnp", mgr.handleUpnpMap)
	mux.HandleFunc("DELETE /api/instances/{id}/upnp", mgr.handleUpnpUnmap)
	mux.HandleFunc("GET /api/instances/{id}/geyser", mgr.handleGeyserGet)
	mux.HandleFunc("POST /api/instances/{id}/geyser", mgr.handleGeyserInstall)
	mux.HandleFunc("DELETE /api/instances/{id}/geyser", mgr.handleGeyserRemove)
	mux.HandleFunc("GET /api/instances/{id}/frp", mgr.handleFrpGet)
	mux.HandleFunc("POST /api/instances/{id}/frp", mgr.handleFrpSet)
	mux.HandleFunc("POST /api/instances/{id}/frp/start", mgr.handleFrpStart)
	mux.HandleFunc("POST /api/instances/{id}/frp/stop", mgr.handleFrpStop)
	mux.HandleFunc("GET /api/instances/{id}/frp/toml", mgr.handleFrpTomlGet)
	mux.HandleFunc("POST /api/instances/{id}/frp/toml", mgr.handleFrpTomlSet)
	mux.HandleFunc("GET /api/instances/{id}/netdoctor", mgr.handleNetDoctor)
	mux.HandleFunc("POST /api/instances/{id}/netfix", mgr.handleNetFix)
	mux.HandleFunc("GET /api/instances/{id}/export", mgr.handleExport)
	mux.HandleFunc("GET /api/instances/{id}/world", mgr.handleWorldGet)
	mux.HandleFunc("POST /api/instances/{id}/world/reset", mgr.handleWorldReset)
	mux.HandleFunc("POST /api/instances/{id}/world/upload", mgr.handleWorldUpload)
	mux.HandleFunc("GET /api/instances/{id}/statuspage", mgr.handleStatusPageGet)
	mux.HandleFunc("POST /api/instances/{id}/statuspage", mgr.handleStatusPageSet)
	mux.HandleFunc("GET /api/instances/{id}/optimize", mgr.handleOptimizeGet)
	mux.HandleFunc("POST /api/instances/{id}/optimize", mgr.handleOptimizeApply)
	mux.HandleFunc("POST /api/instances/{id}/clone", mgr.handleClone)
	mux.HandleFunc("GET /api/frp/sakura/tunnels", mgr.handleSakuraTunnels)
	mux.HandleFunc("GET /api/trash", mgr.handleTrashList)
	mux.HandleFunc("POST /api/trash/{name}/restore", mgr.handleTrashRestore)
	mux.HandleFunc("DELETE /api/trash/{name}", mgr.handleTrashDelete)
	mux.HandleFunc("GET /api/public/status/{slug}", mgr.handlePublicStatus)
	mux.HandleFunc("GET /s/{slug}", mgr.handlePublicPage)
	mux.HandleFunc("GET /api/instances/{id}/icon", mgr.handleIconGet)
	mux.HandleFunc("POST /api/instances/{id}/icon", mgr.handleIconSet)
	mux.HandleFunc("GET /api/instances/{id}/timedcmds", mgr.handleTimedCmdsGet)
	mux.HandleFunc("POST /api/instances/{id}/timedcmds", mgr.handleTimedCmdsSet)
	mux.HandleFunc("GET /api/restart", mgr.handleRestartGet)
	mux.HandleFunc("POST /api/restart", mgr.handleRestartSet)
	mux.HandleFunc("GET /api/bootstart", mgr.handleBootstartGet)
	mux.HandleFunc("POST /api/bootstart", mgr.handleBootstartSet)
	mux.HandleFunc("GET /api/versions", handleVersions)
	mux.HandleFunc("GET /api/loaders", handleLoaderVersions)
	mux.HandleFunc("GET /api/activity", mgr.handleActivity)
	mux.HandleFunc("GET /api/system", mgr.handleSystem)
	mux.HandleFunc("GET /api/stats", mgr.handleSystemStats)

	// 邮件通知
	mux.HandleFunc("GET /api/mail", mgr.handleMailGet)
	mux.HandleFunc("POST /api/mail", mgr.handleMailSet)
	mux.HandleFunc("POST /api/mail/test", mgr.handleMailTest)

	// AI 助手
	mux.HandleFunc("GET /api/ai", mgr.handleAIGet)
	mux.HandleFunc("POST /api/ai", mgr.handleAISet)
	mux.HandleFunc("POST /api/ai/test", mgr.handleAITest)
	mux.HandleFunc("POST /api/ai/models", mgr.handleAIModels)
	mux.HandleFunc("POST /api/instances/{id}/ai/analyze", mgr.handleAIAnalyze)
	mux.HandleFunc("POST /api/instances/{id}/ai/apply", mgr.handleAIApply)

	// 模组市场（Modrinth 代理）
	mux.HandleFunc("GET /api/mods/search", mgr.handleModSearch)
	mux.HandleFunc("GET /api/mods/{slug}/versions", mgr.handleModVersions)
	mux.HandleFunc("POST /api/mods/install", mgr.handleModInstall)
	mux.HandleFunc("GET /api/mods/installs", mgr.handleInstallsList)
	mux.HandleFunc("GET /api/mods/updates", mgr.handleModUpdates)
	mux.HandleFunc("POST /api/mods/update", mgr.handleModUpdate)
	mux.HandleFunc("POST /api/modpack/install", mgr.handleModpackInstall)
	mux.HandleFunc("POST /api/modpack/upload", mgr.handleModpackUpload)

	// 备份
	mux.HandleFunc("GET /api/backups", mgr.handleBackupList)
	mux.HandleFunc("POST /api/instances/{id}/backup", mgr.handleBackupCreate)
	mux.HandleFunc("DELETE /api/backups/{file}", mgr.handleBackupDelete)
	mux.HandleFunc("GET /api/backups/{file}/download", mgr.handleBackupDownload)
	mux.HandleFunc("GET /api/backups/{file}/ls", mgr.handleBackupBrowse)
	mux.HandleFunc("POST /api/backups/{file}/extract", mgr.handleBackupExtract)
	mux.HandleFunc("POST /api/backups/restore", mgr.handleBackupRestore)
	mux.HandleFunc("GET /api/autobackup", mgr.handleAutoBackupGet)
	mux.HandleFunc("POST /api/autobackup", mgr.handleAutoBackupSet)

	// WebDAV 云盘
	mux.HandleFunc("GET /api/webdav", mgr.handleWebDAVGet)
	mux.HandleFunc("POST /api/webdav", mgr.handleWebDAVSet)
	mux.HandleFunc("POST /api/webdav/test", mgr.handleWebDAVTest)
	mux.HandleFunc("GET /api/cloud", mgr.handleCloudList)
	mux.HandleFunc("POST /api/cloud/upload/{file}", mgr.handleCloudUpload)
	mux.HandleFunc("POST /api/cloud/pull/{file}", mgr.handleCloudPull)

	// 面板访问密码
	mux.HandleFunc("GET /api/auth/status", mgr.handleAuthStatus)
	mux.HandleFunc("POST /api/auth/login", mgr.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", mgr.handleLogout)
	mux.HandleFunc("POST /api/auth/set", mgr.handleAuthSet)
	mux.HandleFunc("POST /api/auth/setup", mgr.handleAuthSetup)

	// Quick-Talk 语音房间(Go 版中转;前端构建产物 embed 在 web/talk/)
	talk := NewTalkServer(*dataDir)
	talk.mgr = mgr
	talkSrv = talk
	if *wtPort > 0 {
		talk.wt = NewWTRelay(talk, *dataDir, *wtPort) // UDP/QUIC 低延迟通道,失败自动回落纯 WS
	}
	mux.HandleFunc("GET /talk/ws", talk.HandleWS)
	mux.HandleFunc("GET /talk/health", talk.HandleHealth)
	mux.HandleFunc("GET /api/talk/rooms", talk.HandleRooms)
	mux.HandleFunc("GET /api/instances/{id}/talkpw", mgr.handleTalkPwGet)
	mux.HandleFunc("POST /api/instances/{id}/talkpw", mgr.handleTalkPwSet)

	mux.Handle("/", http.FileServer(http.FS(web.FS)))

	listenHost := *host
	if listenHost == "" {
		listenHost = "127.0.0.1"
	}
	addr := listenHost + ":" + *port

	// HTTPS 监听(自签):局域网访问语音开黑需要 Secure Context 才能用
	// 麦克风/屏幕共享。绑 0.0.0.0 让手机/其他电脑可达;面板 API 仍受
	// auth 中间件保护(开了密码则远端必须登录)。
	go func() {
		certFile, keyFile, err := ensureTLSCert(*dataDir)
		if err != nil {
			log.Printf("TLS 证书生成失败(语音开黑仅本机可用): %v", err)
			return
		}
		httpsAddr := "0.0.0.0:" + *httpsPort
		srv := &http.Server{
			Addr:      httpsAddr,
			Handler:   mgr.authMiddleware(mux),
			TLSConfig: tlsConfig(certFile, keyFile),
		}
		log.Printf("HTTPS listening on https://%s (语音开黑局域网入口)", httpsAddr)
		if err := srv.ListenAndServeTLS("", ""); err != nil {
			log.Printf("HTTPS 启动失败: %v", err)
		}
	}()

	log.Printf("MCS Panel listening on http://%s (data: %s)", addr, *dataDir)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// 端口被占：多半是面板已在运行。GUI 版直接开窗口连上已有实例，
		// 纯 server 版维持原来的直接退出。
		if uiHandlePortBusy("http://127.0.0.1:" + *port) {
			return
		}
		log.Fatal(err)
	}
	go func() {
		log.Fatal(http.Serve(ln, mgr.authMiddleware(mux)))
	}()

	// server 版：阻塞等待；webview 版：打开内嵌浏览器窗口，关窗即退出面板
	runFrontend("http://127.0.0.1:" + *port)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
