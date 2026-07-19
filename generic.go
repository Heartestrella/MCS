package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ===== 通用游戏服务器（SteamCMD / 任意可执行） =====

const steamcmdZip = "https://steamcdn-a.akamaihd.net/client/installer/steamcmd.zip"

// ensureSteamCmd downloads steamcmd into dataDir/steamcmd if missing.
func (m *Manager) ensureSteamCmd(hub *ConsoleHub) (string, error) {
	dir := filepath.Join(m.dataDir, "steamcmd")
	exe := filepath.Join(dir, "steamcmd.exe")
	if _, err := os.Stat(exe); err == nil {
		return exe, nil
	}
	hub.Broadcast("[MCS] 未检测到 SteamCMD，正在自动下载（约 3MB，仅需一次）...")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	zipPath := filepath.Join(dir, "steamcmd.zip")
	if err := downloadTo(steamcmdZip, zipPath); err != nil {
		return "", fmt.Errorf("下载 SteamCMD 失败: %w", err)
	}
	if err := unzip(zipPath, dir); err != nil {
		return "", fmt.Errorf("解压 SteamCMD 失败: %w", err)
	}
	os.Remove(zipPath)
	if _, err := os.Stat(exe); err != nil {
		return "", fmt.Errorf("解压后未找到 steamcmd.exe")
	}
	hub.Broadcast("[MCS] SteamCMD 就绪")
	return exe, nil
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

// steamInstall runs steamcmd to install the app into the instance dir.
func (m *Manager) steamInstall(in *Instance, rs *runtimeState) {
	hub := rs.console
	fail := func(msg string) {
		m.mu.Lock()
		rs.status = "error"
		rs.errMsg = msg
		m.mu.Unlock()
		hub.Broadcast("[MCS] SteamCMD 安装失败: " + msg)
		m.addActivity("orange", fmt.Sprintf("<b>%s</b> SteamCMD 安装失败", in.Name))
	}
	exe, err := m.ensureSteamCmd(hub)
	if err != nil {
		fail(err.Error())
		return
	}
	dir := m.instDir(in.ID)
	hub.Broadcast(fmt.Sprintf("[MCS] 正在下载游戏服务端（AppID %d，取决于游戏大小可能需要较长时间）...", in.SteamAppID))
	cmd := hideWindow(exec.Command(exe,
		"+force_install_dir", dir,
		"+login", "anonymous",
		"+app_update", fmt.Sprint(in.SteamAppID), "validate",
		"+quit"))
	out, err := cmd.StdoutPipe()
	if err != nil {
		fail(err.Error())
		return
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		fail(err.Error())
		return
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := cleanLine(toUTF8(sc.Bytes()))
		if line == "" {
			continue
		}
		// steamcmd 输出多，只转发关键进度行
		if strings.Contains(line, "Update state") || strings.Contains(line, "Success") ||
			strings.Contains(line, "ERROR") || strings.Contains(line, "progress") {
			hub.Broadcast("[SteamCMD] " + line)
		}
	}
	if err := cmd.Wait(); err != nil {
		fail("steamcmd 退出异常: " + err.Error())
		return
	}
	m.mu.Lock()
	rs.status = "stopped"
	m.save()
	m.mu.Unlock()
	hub.Broadcast("[MCS] 游戏服务端安装完成！请到「服务器配置」填写启动命令后启动。")
	hub.Broadcast("[MCS] 提示: 启动命令示例——饥荒: bin64\\dontstarve_dedicated_server_nullrenderer_x64.exe；帕鲁: PalServer.exe")
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
