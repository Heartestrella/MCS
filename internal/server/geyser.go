package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
)

// ===== 基岩版互通（Geyser + Floodgate 一键安装） =====

const geyserURL = "https://download.geysermc.org/v2/projects/geyser/versions/latest/builds/latest/downloads/spigot"
const floodgateURL = "https://download.geysermc.org/v2/projects/floodgate/versions/latest/builds/latest/downloads/spigot"

// geyserStatus reports whether Geyser/Floodgate jars exist in plugins/.
func (m *Manager) geyserStatus(id string) (geyser, floodgate bool) {
	pl := filepath.Join(m.instDir(id), "plugins")
	if ms, _ := filepath.Glob(filepath.Join(pl, "Geyser*.jar")); len(ms) > 0 {
		geyser = true
	}
	if ms, _ := filepath.Glob(filepath.Join(pl, "floodgate*.jar")); len(ms) > 0 {
		floodgate = true
	}
	return
}

func (m *Manager) handleGeyserGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	var typ string
	if ok {
		typ = in.Type
	}
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	g, f := m.geyserStatus(id)
	writeJSON(w, 200, map[string]any{
		"supported": isPaperLike(typ),
		"installed": g && f,
	})
}

func (m *Manager) handleGeyserInstall(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	if !isPaperLike(in.Type) {
		writeErr(w, 400, "基岩互通目前仅支持 Paper / Purpur 服务端")
		return
	}
	rs := m.getRTSafe(id)
	go func() {
		hub := rs.console
		pl := filepath.Join(m.instDir(id), "plugins")
		os.MkdirAll(pl, 0755)
		hub.Broadcast("[MCS] 正在下载 Geyser（基岩版协议转换）...")
		if err := downloadTo(geyserURL, filepath.Join(pl, "Geyser-Spigot.jar")); err != nil {
			hub.Broadcast("[MCS] Geyser 下载失败: " + err.Error())
			m.addActivity("orange", fmt.Sprintf("<b>%s</b> 基岩互通安装失败", in.Name))
			return
		}
		hub.Broadcast("[MCS] 正在下载 Floodgate（免正版账号登录）...")
		if err := downloadTo(floodgateURL, filepath.Join(pl, "floodgate-spigot.jar")); err != nil {
			hub.Broadcast("[MCS] Floodgate 下载失败: " + err.Error())
			m.addActivity("orange", fmt.Sprintf("<b>%s</b> 基岩互通安装失败", in.Name))
			return
		}
		hub.Broadcast("[MCS] 基岩互通已安装！重启服务器后，手机版用「服务器地址 + 端口 19132 (UDP)」即可进入。")
		m.addActivity("green", fmt.Sprintf("<b>%s</b> 已开启基岩版互通（重启生效）", in.Name))
		m.notify("基岩版互通已安装", fmt.Sprintf("世界「%s」已安装 Geyser + Floodgate。重启服务器后，手机/主机版玩家用 UDP 端口 19132 即可进入。", in.Name))
	}()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleGeyserRemove deletes Geyser/Floodgate jars (and their config dirs stay).
func (m *Manager) handleGeyserRemove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	pl := filepath.Join(m.instDir(id), "plugins")
	removed := 0
	for _, pat := range []string{"Geyser*.jar", "Geyser*.jar.disabled", "floodgate*.jar", "floodgate*.jar.disabled"} {
		if ms, _ := filepath.Glob(filepath.Join(pl, pat)); len(ms) > 0 {
			for _, p := range ms {
				if os.Remove(p) == nil {
					removed++
				}
			}
		}
	}
	if removed == 0 {
		writeErr(w, 400, "未安装基岩互通")
		return
	}
	m.addActivity("blue", fmt.Sprintf("<b>%s</b> 已关闭基岩版互通（重启生效）", in.Name))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// getRTSafe locks and returns the runtime state for an instance.
func (m *Manager) getRTSafe(id string) *runtimeState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getRT(id)
}
