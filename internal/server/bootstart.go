package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// ===== 开机自启 =====
// 面板：HKCU\...\Run 注册表项（无需管理员权限）。
// 世界：Instance.AutoStart，面板启动后自动拉起。

const bootRunKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
const bootRunName = "MCSPanel"

func regQueryBootstart() bool {
	out, err := hideWindow(exec.Command("reg", "query", bootRunKey, "/v", bootRunName)).CombinedOutput()
	return err == nil && strings.Contains(string(out), bootRunName)
}

func (m *Manager) handleBootstartGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]bool{"enabled": regQueryBootstart()})
}

func (m *Manager) handleBootstartSet(w http.ResponseWriter, r *http.Request) {
	if runtime.GOOS != "windows" {
		writeErr(w, 400, "开机自启目前仅支持 Windows;Linux 请使用 systemd 服务")
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if body.Enabled {
		exe, err := os.Executable()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		// 隐藏窗口启动：注册表直接跑 exe 会闪一个控制台窗，用 conhost --headless 不通用，
		// 简单起见直接注册 exe —— 面板本来就是控制台程序，最小化即可。
		if out, err := hideWindow(exec.Command("reg", "add", bootRunKey, "/v", bootRunName,
			"/t", "REG_SZ", "/d", `"`+exe+`"`, "/f")).CombinedOutput(); err != nil {
			writeErr(w, 500, "写入注册表失败: "+toUTF8(out))
			return
		}
		m.addActivity("green", "面板开机自启已开启")
	} else {
		hideWindow(exec.Command("reg", "delete", bootRunKey, "/v", bootRunName, "/f")).Run()
		m.addActivity("blue", "面板开机自启已关闭")
	}
	writeJSON(w, 200, map[string]bool{"ok": true, "enabled": body.Enabled})
}

// autoStartInstances launches instances marked AutoStart shortly after boot.
func (m *Manager) autoStartInstances() {
	go func() {
		time.Sleep(5 * time.Second)
		m.mu.Lock()
		var targets []*Instance
		for _, in := range m.insts {
			if in.AutoStart {
				targets = append(targets, in)
			}
		}
		m.mu.Unlock()
		for _, in := range targets {
			if err := m.startInstance(in); err != nil {
				m.addActivity("orange", fmt.Sprintf("<b>%s</b> 自动启动失败: %s", in.Name, err.Error()))
				continue
			}
			m.addActivity("green", fmt.Sprintf("<b>%s</b> 随面板自动启动", in.Name))
			// 串行拉起，避免多个实例同时下载 Java/抢 IO
			time.Sleep(3 * time.Second)
		}
	}()
}
