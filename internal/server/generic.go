package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ===== 通用游戏服务器（SteamCMD / 任意可执行） =====

// steamcmdSrc returns the platform download URL and the launcher filename.
func steamcmdSrc() (url, launcher string) {
	switch runtime.GOOS {
	case "windows":
		return "https://steamcdn-a.akamaihd.net/client/installer/steamcmd.zip", "steamcmd.exe"
	case "darwin":
		return "https://steamcdn-a.akamaihd.net/client/installer/steamcmd_osx.tar.gz", "steamcmd.sh"
	default:
		return "https://steamcdn-a.akamaihd.net/client/installer/steamcmd_linux.tar.gz", "steamcmd.sh"
	}
}

// ensureSteamCmd downloads steamcmd into dataDir/steamcmd if missing.
// A smoke test (+quit) runs on every first use per panel session; a corrupt
// or partially-extracted install is wiped and re-downloaded once.
func (m *Manager) ensureSteamCmd(hub *ConsoleHub) (string, error) {
	dir := filepath.Join(m.dataDir, "steamcmd")
	srcURL, launcher := steamcmdSrc()
	exe := filepath.Join(dir, launcher)
	if _, err := os.Stat(exe); err == nil {
		if err := steamcmdSmokeTest(exe); err == nil {
			return exe, nil
		}
		hub.Broadcast("[MCS] 检测到 SteamCMD 已损坏，正在重新安装...")
		os.RemoveAll(dir)
	}
	hub.Broadcast("[MCS] 未检测到 SteamCMD，正在自动下载（约 3MB，仅需一次）...")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	zipPath := filepath.Join(dir, "steamcmd"+filepath.Ext(srcURL))
	if strings.HasSuffix(srcURL, ".tar.gz") {
		zipPath = filepath.Join(dir, "steamcmd.tar.gz")
	}
	if err := downloadTo(srcURL, zipPath); err != nil {
		return "", fmt.Errorf("下载 SteamCMD 失败: %w", err)
	}
	if err := extractArchive(zipPath, dir); err != nil {
		return "", fmt.Errorf("解压 SteamCMD 失败: %w", err)
	}
	os.Remove(zipPath)
	if _, err := os.Stat(exe); err != nil {
		return "", fmt.Errorf("解压后未找到 %s", launcher)
	}
	if runtime.GOOS != "windows" {
		os.Chmod(exe, 0755)
	}
	hub.Broadcast("[MCS] SteamCMD 首次自更新中（可能需要 1-2 分钟）...")
	if err := steamcmdSmokeTest(exe); err != nil {
		return "", fmt.Errorf("SteamCMD 自检失败: %v（Linux 需要 lib32gcc-s1 等 32 位库）", err)
	}
	hub.Broadcast("[MCS] SteamCMD 就绪")
	return exe, nil
}

// steamcmdSmokeTest runs `steamcmd +quit` to trigger self-update and verify it works.
func steamcmdSmokeTest(exe string) error {
	cmd := hideWindow(exec.Command(exe, "+quit"))
	cmd.Dir = filepath.Dir(exe)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// steamcmd 自更新后可能以非 0 退出（exit 7），只要产出了核心文件就算可用
		if _, e := os.Stat(filepath.Join(filepath.Dir(exe), "steamclient.dll")); e == nil {
			return nil
		}
		if _, e := os.Stat(filepath.Join(filepath.Dir(exe), "linux32")); e == nil {
			return nil
		}
		tail := string(out)
		if len(tail) > 300 {
			tail = tail[len(tail)-300:]
		}
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(tail))
	}
	return nil
}

// handleGenericCreate creates a generic (non-Minecraft) server instance.
// mode=steamcmd: 用 SteamCMD 安装 appId 到实例目录，装完需用户填启动命令
// mode=custom:   直接使用用户给的启动命令（可选 workDir 留空 = 实例目录）
func (m *Manager) handleGenericCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name       string `json:"name"`
		Mode       string `json:"mode"` // steamcmd / custom
		SteamAppID int    `json:"steamAppId"`
		ExecCmd    string `json:"execCmd"`
		StopCmd    string `json:"stopCmd"`
		Port       int    `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		writeErr(w, 400, "参数不完整")
		return
	}
	if body.Mode == "steamcmd" && body.SteamAppID <= 0 {
		writeErr(w, 400, "请填写 Steam 应用 ID（如饥荒联机版 343050、幻兽帕鲁 2394010）")
		return
	}
	if body.Mode == "custom" && strings.TrimSpace(body.ExecCmd) == "" {
		writeErr(w, 400, "请填写启动命令")
		return
	}
	if body.Port <= 0 {
		body.Port = 27015
	}

	in := &Instance{
		ID:         newID(),
		Name:       body.Name,
		Type:       "generic",
		Port:       body.Port,
		ExecCmd:    body.ExecCmd,
		StopCmd:    body.StopCmd,
		SteamAppID: body.SteamAppID,
		CreatedAt:  time.Now(),
	}
	if err := os.MkdirAll(m.instDir(in.ID), 0755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	m.mu.Lock()
	m.insts[in.ID] = in
	rs := m.getRT(in.ID)
	if body.Mode == "steamcmd" {
		rs.status = "downloading"
		rs.console.ClearProgress()
	} else {
		rs.status = "stopped"
	}
	m.save()
	m.mu.Unlock()

	if body.Mode == "steamcmd" {
		m.addActivity("blue", fmt.Sprintf("正在通过 SteamCMD 安装 <b>%s</b>（AppID %d）", in.Name, in.SteamAppID))
		go m.steamInstall(in, rs)
	} else {
		m.addActivity("green", fmt.Sprintf("已创建通用服务器 <b>%s</b>", in.Name))
	}
	writeJSON(w, 201, m.snapshotLocked(in))
}

// steamInstallLogPath is where the raw steamcmd output for an instance install lives.
func (m *Manager) steamInstallLogPath(instID string) string {
	return filepath.Join(m.instDir(instID), "steamcmd-install.log")
}

// explainSteamcmdFailure 把 steamcmd 的原始报错翻译成对普通用户有指导意义的信息。
func explainSteamcmdFailure(rawLine string, exitCode, appID int) string {
	line := rawLine
	base := fmt.Sprintf("（exit=%d）%s", exitCode, rawLine)
	switch {
	case strings.Contains(line, "Missing configuration"):
		hint := ""
		if s := knownDedicatedServer(appID); s != "" {
			hint = fmt.Sprintf("\n  → 你填的 AppID %d 是「%s」的游戏本体，服务端 AppID 应该是 %s。", appID, s, s)
		}
		return fmt.Sprintf("SteamCMD 拒绝安装 AppID %d（Missing configuration）：\n"+
			"  这个 AppID 通常是游戏本体，匿名 SteamCMD 只能下载「专用服务器」类型的 App。%s\n"+
			"  查正确 AppID：https://steamdb.info 搜「游戏名 dedicated server」\n"+
			"  常见服务端 AppID：饥荒=343050 · 幻兽帕鲁=2394010 · CS2=730 · 泰拉瑞亚=105600 · L4D2=222860 · Valheim=896660 · Rust=258550 · 7DTD=294420 · ARK=376030", appID, hint)
	case strings.Contains(line, "Login Failure") || strings.Contains(line, "Invalid Password") ||
		strings.Contains(line, "Steam Guard code") || strings.Contains(line, "Two-factor code"):
		return "SteamCMD 需要 Steam 账号登录，而当前只支持匿名下载：" + base + "\n  这个 AppID 大概率需要账号（付费或有令牌保护），面板暂不支持。"
	case strings.Contains(line, "No subscription"):
		return "当前账号（匿名）没有此 AppID 的授权：" + base + "\n  → AppID 可能填错，或该服务端需要付费账号。"
	case strings.Contains(line, "Rate Limit"):
		return "被 Steam 限速，稍后重试：" + base
	case strings.Contains(line, "Timeout downloading") || strings.Contains(line, "Timeout"):
		return "下载超时，检查网络/代理：" + base
	case strings.Contains(line, "disk write failure") ||
		strings.Contains(line, "Update state (0x602)") || strings.Contains(line, "Update state (0x606)"):
		return "磁盘写入失败（可能空间不足或权限问题）：" + base
	case strings.Contains(line, "Manifest not available"):
		return "Steam 服务端未返回清单（AppID 可能已下架或临时不可用）：" + base
	default:
		return "检测到失败输出" + base
	}
}

// knownDedicatedServer 若给的 AppID 是游戏本体，返回对应专用服务器 AppID 的字符串描述。
func knownDedicatedServer(appID int) string {
	m := map[int]string{
		550:     "222860",  // L4D2 → L4D2 Dedicated Server
		4000:    "4020",    // Garry's Mod → GMod DS
		107410:  "233780",  // ARMA 3 → ARMA 3 DS
		221100:  "258550",  // DayZ → Rust? 实际 DayZ 服端 = 223350，这里改
		251570:  "294420",  // 7 Days to Die → 7DTD DS
		252950:  "329350",  // Rocket League 无匿名 DS，占位不映射
		304930:  "306110",  // Unturned → Unturned DS
		346110:  "376030",  // ARK: SE → ARK DS
		393380:  "298740",  // Squad → Squad DS(要账号)
		578080:  "236110",  // PUBG 无 DS
		892970:  "896660",  // Valheim → Valheim DS
		1172470: "1172380", // Apex Legends 无 DS
		1621890: "2394010", // Palworld → Palworld Dedicated Server
	}
	// DayZ 修正
	m[221100] = "223350"
	if v, ok := m[appID]; ok {
		return v
	}
	return ""
}

// steamcmdFailurePatterns 用于把命令看起来正常退出、实际却装挂了的情况识别出来。
var steamcmdFailurePatterns = []string{
	"ERROR!",
	"Login Failure",
	"Invalid Password",
	"No subscription",
	"Rate Limit",
	"Timeout downloading",
	"Failed to install",
	"failed to install",
	"Update state (0x602)", // 磁盘满
	"Update state (0x606)", // 磁盘错误
	"Missing update",
	"No app info",
	"App state",
	"disk write failure",
	"Manifest not available",
	"Steam Guard code",
	"Two-factor code",
}

// steamInstall runs steamcmd to install the app into the instance dir.
// 全量输出既写文件日志（<instDir>/steamcmd-install.log），也转发到 WebSocket 控制台，
// 失败关键词命中会作为最终错误上报。
func (m *Manager) steamInstall(in *Instance, rs *runtimeState) {
	hub := rs.console
	logPath := m.steamInstallLogPath(in.ID)
	os.MkdirAll(filepath.Dir(logPath), 0755)
	logFile, ferr := os.Create(logPath)
	if ferr != nil {
		hub.Broadcast("[MCS] 警告：无法创建安装日志文件: " + ferr.Error())
	}
	writeLog := func(line string) {
		if logFile != nil {
			logFile.WriteString(line + "\n")
		}
	}
	defer func() {
		if logFile != nil {
			logFile.Close()
		}
	}()

	fail := func(msg string) {
		m.mu.Lock()
		rs.status = "error"
		rs.errMsg = msg
		m.mu.Unlock()
		hub.Broadcast("[MCS] SteamCMD 安装失败: " + msg)
		hub.Broadcast(fmt.Sprintf("[MCS] 完整日志: %s", logPath))
		writeLog("[MCS-FAIL] " + msg)
		m.addActivity("orange", fmt.Sprintf("<b>%s</b> SteamCMD 安装失败", in.Name))
	}

	writeLog(fmt.Sprintf("[MCS] === SteamCMD install started at %s ===", time.Now().Format(time.RFC3339)))
	writeLog(fmt.Sprintf("[MCS] Instance=%s AppID=%d OS=%s", in.Name, in.SteamAppID, runtime.GOOS))

	exe, err := m.ensureSteamCmd(hub)
	if err != nil {
		fail(err.Error())
		return
	}
	writeLog("[MCS] steamcmd exe: " + exe)

	dir := m.instDir(in.ID)
	hub.Broadcast(fmt.Sprintf("[MCS] 正在下载游戏服务端（AppID %d，取决于游戏大小可能需要较长时间）...", in.SteamAppID))
	hub.Broadcast(fmt.Sprintf("[MCS] 完整安装日志实时写入: %s", logPath))
	args := []string{
		"+@ShutdownOnFailedCommand", "1",
		"+@NoPromptForPassword", "1",
		"+force_install_dir", dir,
		"+login", "anonymous",
		"+app_update", fmt.Sprint(in.SteamAppID), "validate",
		"+quit",
	}
	writeLog("[MCS] cmdline: " + exe + " " + strings.Join(args, " "))
	cmd := hideWindow(exec.Command(exe, args...))
	cmd.Dir = filepath.Dir(exe)

	out, err := cmd.StdoutPipe()
	if err != nil {
		fail("创建 stdout 管道失败: " + err.Error())
		return
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		fail("启动 steamcmd 失败: " + err.Error())
		return
	}

	// 转发所有输出：控制台可以完整看到，日志文件保留全文
	var (
		firstFailLine string
		sawSuccess    bool
		lineCount     int
		lastBroadcast time.Time
	)
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		raw := toUTF8(sc.Bytes())
		line := cleanLine(raw)
		if line == "" {
			continue
		}
		lineCount++
		writeLog(line)

		// 失败特征
		if firstFailLine == "" {
			for _, pat := range steamcmdFailurePatterns {
				if strings.Contains(line, pat) {
					firstFailLine = line
					break
				}
			}
		}
		if strings.Contains(line, "Success! App '") && strings.Contains(line, "fully installed") {
			sawSuccess = true
		}

		// 广播策略：关键行全播，普通行做节流避免刷屏
		key := strings.Contains(line, "Update state") ||
			strings.Contains(line, "Success") ||
			strings.Contains(line, "ERROR") ||
			strings.Contains(line, "Warning") ||
			strings.Contains(line, "Failed") ||
			strings.Contains(line, "Login") ||
			strings.Contains(line, "Steam>") ||
			strings.Contains(line, "downloading") ||
			strings.Contains(line, "verifying") ||
			strings.Contains(line, "preallocating")
		if key || time.Since(lastBroadcast) > 500*time.Millisecond {
			hub.Broadcast("[SteamCMD] " + line)
			lastBroadcast = time.Now()
		}
	}
	if scErr := sc.Err(); scErr != nil && scErr != io.EOF {
		writeLog("[MCS] scanner error: " + scErr.Error())
	}

	waitErr := cmd.Wait()
	exitCode := 0
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			fail("等待 steamcmd 结束失败: " + waitErr.Error())
			return
		}
	}
	writeLog(fmt.Sprintf("[MCS] steamcmd exited: code=%d, lines=%d", exitCode, lineCount))

	if firstFailLine != "" {
		fail(explainSteamcmdFailure(firstFailLine, exitCode, in.SteamAppID))
		return
	}
	if exitCode != 0 && !sawSuccess {
		fail(fmt.Sprintf("steamcmd 异常退出（exit code %d），请查看完整日志排查", exitCode))
		return
	}
	// 空目录 = 装挂了但没报错
	entries, _ := os.ReadDir(dir)
	nonLog := 0
	for _, e := range entries {
		n := e.Name()
		if n == "steamcmd-install.log" || strings.HasPrefix(n, ".") {
			continue
		}
		nonLog++
	}
	if nonLog == 0 {
		fail("安装完成但目标目录为空，通常是 AppID 不支持匿名下载或该 AppID 无专用服务器（需要 Steam 账号或用错了 ID）")
		return
	}

	m.mu.Lock()
	rs.status = "stopped"
	m.save()
	m.mu.Unlock()
	hub.Broadcast("[MCS] 游戏服务端安装完成！请到「服务器配置」填写启动命令后启动。")
	hub.Broadcast("[MCS] 提示: 启动命令示例——饥荒: bin64\\dontstarve_dedicated_server_nullrenderer_x64.exe；帕鲁: PalServer.exe")
	writeLog("[MCS] === install ok ===")
	m.addActivity("green", fmt.Sprintf("<b>%s</b>（AppID %d）安装完成，配置启动命令后即可开服", in.Name, in.SteamAppID))
	m.notify("SteamCMD 安装完成", fmt.Sprintf("通用服务器「%s」（AppID %d）已安装完成。到面板「服务器配置」填写启动命令后即可启动。", in.Name, in.SteamAppID))
}

// handleGenericUpdate edits exec/stop commands and working dir of a generic instance.
func (m *Manager) handleGenericUpdate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ExecCmd *string `json:"execCmd"`
		StopCmd *string `json:"stopCmd"`
		WorkDir *string `json:"workDir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if body.WorkDir != nil {
		wd := strings.TrimSpace(*body.WorkDir)
		if wd != "" {
			if st, err := os.Stat(wd); err != nil || !st.IsDir() {
				writeErr(w, 400, "根目录不存在或不是文件夹: "+wd)
				return
			}
		}
		*body.WorkDir = wd
	}
	m.mu.Lock()
	in, ok := m.insts[r.PathValue("id")]
	if !ok || in.Type != "generic" {
		m.mu.Unlock()
		writeErr(w, 404, "通用实例不存在")
		return
	}
	if body.ExecCmd != nil {
		in.ExecCmd = *body.ExecCmd
	}
	if body.StopCmd != nil {
		in.StopCmd = *body.StopCmd
	}
	if body.WorkDir != nil {
		in.WorkDir = *body.WorkDir
	}
	m.save()
	snap := m.snapshot(in)
	m.mu.Unlock()
	writeJSON(w, 200, snap)
}
