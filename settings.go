package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ===== server.properties 可视化编辑 =====

// 常用键的说明与类型（前端渲染友好表单用；其余键归入"高级"）
var propMeta = []struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Kind  string `json:"kind"` // bool / int / text / select
	Opts  string `json:"opts,omitempty"`
}{
	{"motd", "服务器介绍（MOTD）", "text", ""},
	{"max-players", "最大玩家数", "int", ""},
	{"gamemode", "默认游戏模式", "select", "survival,creative,adventure,spectator"},
	{"difficulty", "难度", "select", "peaceful,easy,normal,hard"},
	{"pvp", "允许 PVP", "bool", ""},
	{"online-mode", "正版验证", "bool", ""},
	{"white-list", "开启白名单", "bool", ""},
	{"spawn-protection", "出生点保护半径", "int", ""},
	{"view-distance", "视距（区块）", "int", ""},
	{"simulation-distance", "模拟距离（区块）", "int", ""},
	{"allow-flight", "允许飞行（防踢挂机）", "bool", ""},
	{"allow-nether", "开启下界", "bool", ""},
	{"hardcore", "极限模式", "bool", ""},
	{"level-seed", "世界种子（仅新世界生效）", "text", ""},
	{"server-port", "端口（改后需重启）", "int", ""},
}

func (m *Manager) propsPath(id string) string {
	return filepath.Join(m.instDir(id), "server.properties")
}

// readProps parses server.properties preserving only k=v lines.
func readProps(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		if i := strings.IndexByte(line, '='); i > 0 {
			out[line[:i]] = line[i+1:]
		}
	}
	return out, sc.Err()
}

func (m *Manager) handlePropsGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	_, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	props, err := readProps(m.propsPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, 200, map[string]any{"props": map[string]string{}, "meta": propMeta})
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"props": props, "meta": propMeta})
}

// handlePropsSet merges submitted keys into server.properties (rewrites file sorted).
func (m *Manager) handlePropsSet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	var name string
	if ok {
		name = in.Name
	}
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	props, err := readProps(m.propsPath(id))
	if err != nil && !os.IsNotExist(err) {
		writeErr(w, 500, err.Error())
		return
	}
	if props == nil {
		props = map[string]string{}
	}
	for k, v := range body {
		k = strings.TrimSpace(k)
		if k == "" || strings.ContainsAny(k, "=\n\r") || strings.ContainsAny(v, "\n\r") {
			continue
		}
		props[k] = v
	}
	// port 同步回实例元数据
	if p, ok := props["server-port"]; ok {
		var port int
		fmt.Sscanf(p, "%d", &port)
		if port > 0 {
			m.mu.Lock()
			in.Port = port
			m.save()
			m.mu.Unlock()
		}
	}

	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString("# Edited by MCS Panel " + time.Now().Format("2006-01-02 15:04:05") + "\n")
	for _, k := range keys {
		sb.WriteString(k + "=" + props[k] + "\n")
	}
	if err := os.WriteFile(m.propsPath(id), []byte(sb.String()), 0644); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	m.addActivity("blue", fmt.Sprintf("世界 <b>%s</b> 的服务器设置已修改", name))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ===== 定时重启 =====

type RestartConfig struct {
	Enabled bool   `json:"enabled"`
	Time    string `json:"time"`   // "HH:MM" 每天定时
	Warn    bool   `json:"warn"`   // 提前 1 分钟游戏内公告
	Backup  bool   `json:"backup"` // 重启前先备份
}

func (m *Manager) restartConfigPath() string { return filepath.Join(m.dataDir, "restart.json") }

func (m *Manager) loadRestart() RestartConfig {
	cfg := RestartConfig{Time: "04:00", Warn: true}
	if b, err := os.ReadFile(m.restartConfigPath()); err == nil {
		json.Unmarshal(b, &cfg)
	}
	return cfg
}

func (m *Manager) handleRestartGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, m.loadRestart())
}

func (m *Manager) handleRestartSet(w http.ResponseWriter, r *http.Request) {
	var cfg RestartConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if _, err := time.Parse("15:04", cfg.Time); err != nil {
		writeErr(w, 400, "时间格式应为 HH:MM，如 04:00")
		return
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(m.restartConfigPath(), b, 0644); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	m.addActivity("blue", fmt.Sprintf("定时重启已%s（每天 %s）", map[bool]string{true: "开启", false: "关闭"}[cfg.Enabled], cfg.Time))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// startScheduledRestart checks every 30s whether the daily restart time hit.
func (m *Manager) startScheduledRestart() {
	go func() {
		var lastDay string // 防止同一天重复触发
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for range tick.C {
			cfg := m.loadRestart()
			if !cfg.Enabled {
				continue
			}
			now := time.Now()
			if now.Format("15:04") != cfg.Time || now.Format("2006-01-02") == lastDay {
				continue
			}
			lastDay = now.Format("2006-01-02")
			m.runScheduledRestart(cfg)
		}
	}()
}

func (m *Manager) runScheduledRestart(cfg RestartConfig) {
	m.mu.Lock()
	var targets []*Instance
	for id, in := range m.insts {
		if rs, ok := m.rt[id]; ok && rs.status == "running" {
			targets = append(targets, in)
		}
	}
	m.mu.Unlock()
	if len(targets) == 0 {
		return
	}
	m.addActivity("blue", fmt.Sprintf("定时重启开始（%d 个运行中的世界）", len(targets)))

	for _, in := range targets {
		if cfg.Warn {
			m.sendCommand(in, "say [MCS] 服务器将在 60 秒后定时重启")
			time.Sleep(60 * time.Second)
		}
		if cfg.Backup {
			if _, _, err := m.doBackup(in.ID); err != nil {
				m.addActivity("orange", "定时重启前备份失败: "+err.Error())
			}
		}
		if err := m.stopInstance(in); err != nil {
			continue
		}
		// 等待完全停止（最多 60 秒）
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(2 * time.Second)
			m.mu.Lock()
			st := m.getRT(in.ID).status
			m.mu.Unlock()
			if st == "stopped" || st == "error" {
				break
			}
		}
		if err := m.startInstance(in); err != nil {
			m.addActivity("orange", fmt.Sprintf("<b>%s</b> 定时重启失败: %s", in.Name, err.Error()))
		} else {
			m.addActivity("green", fmt.Sprintf("<b>%s</b> 定时重启完成", in.Name))
		}
	}
	m.notify("定时重启完成", fmt.Sprintf("已完成 %d 个世界的每日定时重启。", len(targets)))
}
