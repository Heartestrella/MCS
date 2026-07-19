package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
)

// ===== Paper 跨版本升级 =====
// 现有 core/update 只升 build；这里支持 1.20.x → 1.21.x 跨游戏版本升级。
// 世界会被新版本自动升格（不可逆），所以强制推荐先备份。

// handleUpgradeVersions lists stable Paper versions newer than the instance's.
func (m *Manager) handleUpgradeVersions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	if in.Type != "paper" {
		writeErr(w, 400, "目前只支持 Paper 服务端跨版本升级")
		return
	}
	all, err := paperVersions()
	if err != nil {
		writeErr(w, 502, "获取版本列表失败: "+err.Error())
		return
	}
	var newer []string
	for _, v := range all {
		if versionLess(in.Version, v) {
			newer = append(newer, v)
		}
	}
	writeJSON(w, 200, map[string]any{"current": in.Version, "newer": newer})
}

// handleUpgrade downloads the target version jar and switches the instance to it.
func (m *Manager) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Version string `json:"version"`
		Backup  bool   `json:"backup"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Version == "" {
		writeErr(w, 400, "参数不完整")
		return
	}
	m.mu.Lock()
	in, ok := m.insts[id]
	if !ok {
		m.mu.Unlock()
		writeErr(w, 404, "实例不存在")
		return
	}
	rs := m.getRT(id)
	if rs.status != "stopped" && rs.status != "error" {
		m.mu.Unlock()
		writeErr(w, 409, "请先停止服务器再升级版本")
		return
	}
	if in.Type != "paper" {
		m.mu.Unlock()
		writeErr(w, 400, "目前只支持 Paper 服务端跨版本升级")
		return
	}
	if !versionLess(in.Version, body.Version) {
		m.mu.Unlock()
		writeErr(w, 400, "只能升级到更高版本（世界格式升级后无法回退到旧版本）")
		return
	}
	oldJar := in.JarFile
	oldVer := in.Version
	rs.status = "downloading"
	rs.console.ClearProgress()
	name := in.Name
	m.mu.Unlock()

	go func() {
		fail := func(msg string) {
			m.mu.Lock()
			rs.status = "stopped"
			m.mu.Unlock()
			rs.console.Broadcast("[MCS] 版本升级失败: " + msg)
			m.addActivity("orange", fmt.Sprintf("<b>%s</b> 版本升级失败", name))
		}
		if body.Backup {
			rs.console.Broadcast("[MCS] 升级前备份中 ...")
			if _, _, err := m.doBackup(id); err != nil {
				fail("升级前备份失败，已取消: " + err.Error())
				return
			}
		}
		dir := m.instDir(id)
		newJar, err := downloadPaper(body.Version, dir, rs.console)
		if err != nil {
			fail(err.Error())
			return
		}
		m.mu.Lock()
		in.Version = body.Version
		in.JarFile = newJar
		in.JavaPath = "" // 让面板按新版本重新匹配 Java
		rs.status = "stopped"
		m.save()
		m.mu.Unlock()
		if oldJar != "" && oldJar != newJar {
			os.Remove(filepath.Join(dir, oldJar))
		}
		rs.console.Broadcast(fmt.Sprintf("[MCS] 已从 %s 升级到 %s，启动后世界会自动升格（首次启动稍慢）", oldVer, body.Version))
		m.addActivity("green", fmt.Sprintf("<b>%s</b> 已升级 %s → %s", name, oldVer, body.Version))
		m.notify("版本升级完成", fmt.Sprintf("世界「%s」已从 Minecraft %s 升级到 %s。\n首次启动会自动升格世界数据，耗时比平常久，属正常现象。\n注意：升级后无法降回旧版本。", name, oldVer, body.Version))
	}()
	writeJSON(w, 200, map[string]bool{"ok": true})
}
