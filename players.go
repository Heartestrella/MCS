package main

import (
	"compress/gzip"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ===== 玩家管理：白名单 / OP / 封禁 =====

// offlineUUID computes the offline-mode UUID Minecraft uses:
// UUID v3 (MD5) of "OfflinePlayer:<name>".
func offlineUUID(name string) string {
	h := md5.Sum([]byte("OfflinePlayer:" + name))
	h[6] = (h[6] & 0x0f) | 0x30 // version 3
	h[8] = (h[8] & 0x3f) | 0x80 // variant
	x := fmt.Sprintf("%x", h)
	return x[0:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:32]
}

type playerEntry struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

func readPlayerJSON(path string) []map[string]any {
	var out []map[string]any
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &out)
	}
	return out
}

func namesOf(list []map[string]any) []string {
	out := make([]string, 0, len(list))
	for _, e := range list {
		if n, ok := e["name"].(string); ok {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func (m *Manager) handlePlayersGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	_, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	dir := m.instDir(id)
	props, _ := readProps(m.propsPath(id))
	writeJSON(w, 200, map[string]any{
		"whitelist":   namesOf(readPlayerJSON(filepath.Join(dir, "whitelist.json"))),
		"ops":         namesOf(readPlayerJSON(filepath.Join(dir, "ops.json"))),
		"bans":        namesOf(readPlayerJSON(filepath.Join(dir, "banned-players.json"))),
		"whitelistOn": props["white-list"] == "true",
	})
}

// handlePlayersAction applies a whitelist/op/ban action. Running server →
// console command (takes effect immediately); stopped → edit the JSON file.
func (m *Manager) handlePlayersAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Action string `json:"action"`
		Name   string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		writeErr(w, 400, "参数不完整")
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if len(body.Name) > 16 || strings.ContainsAny(body.Name, " /\\\"'\n\r\t") {
		writeErr(w, 400, "玩家名不合法")
		return
	}

	m.mu.Lock()
	in, ok := m.insts[id]
	var running bool
	var name string
	if ok {
		name = in.Name
		rs := m.getRT(id)
		running = rs.status == "running"
	}
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}

	cmds := map[string]string{
		"whitelist-add":    "whitelist add %s",
		"whitelist-remove": "whitelist remove %s",
		"op":               "op %s",
		"deop":             "deop %s",
		"ban":              "ban %s",
		"pardon":           "pardon %s",
		"kick":             "kick %s",
	}
	tmpl, ok := cmds[body.Action]
	if !ok {
		writeErr(w, 400, "未知操作")
		return
	}

	if running {
		if err := m.sendCommand(in, fmt.Sprintf(tmpl, body.Name)); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	} else {
		if body.Action == "kick" {
			writeErr(w, 409, "服务器未运行，无法踢人")
			return
		}
		if err := m.editPlayerFile(id, body.Action, body.Name); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	m.addActivity("blue", fmt.Sprintf("<b>%s</b>：对玩家 <b>%s</b> 执行了 %s", name, body.Name, body.Action))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// editPlayerFile updates whitelist/ops/banned-players JSON while server is stopped.
func (m *Manager) editPlayerFile(id, action, player string) error {
	dir := m.instDir(id)
	var file string
	add := false
	extra := map[string]any{}
	switch action {
	case "whitelist-add":
		file, add = "whitelist.json", true
	case "whitelist-remove":
		file = "whitelist.json"
	case "op":
		file, add = "ops.json", true
		extra["level"] = 4
		extra["bypassesPlayerLimit"] = false
	case "deop":
		file = "ops.json"
	case "ban":
		file, add = "banned-players.json", true
		extra["created"] = "1970-01-01 00:00:00 +0000"
		extra["source"] = "MCS Panel"
		extra["expires"] = "forever"
		extra["reason"] = "Banned by an operator."
	case "pardon":
		file = "banned-players.json"
	}
	p := filepath.Join(dir, file)
	list := readPlayerJSON(p)
	// remove existing entry for this name either way
	out := list[:0]
	for _, e := range list {
		if n, _ := e["name"].(string); !strings.EqualFold(n, player) {
			out = append(out, e)
		}
	}
	if add {
		entry := map[string]any{"uuid": offlineUUID(player), "name": player}
		for k, v := range extra {
			entry[k] = v
		}
		out = append(out, entry)
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return os.WriteFile(p, b, 0644)
}

// ===== 日志查看 =====

func (m *Manager) handleLogsList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	_, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	type logFile struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	out := []logFile{}
	entries, _ := os.ReadDir(filepath.Join(m.instDir(id), "logs"))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasSuffix(n, ".log") && !strings.HasSuffix(n, ".log.gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, logFile{n, info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	writeJSON(w, 200, out)
}

const logTailBytes = 512 * 1024 // 每次最多返回 512KB 文本

func (m *Manager) handleLogGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	file := r.PathValue("file")
	m.mu.Lock()
	_, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	if strings.ContainsAny(file, `/\`) || strings.Contains(file, "..") ||
		(!strings.HasSuffix(file, ".log") && !strings.HasSuffix(file, ".log.gz")) {
		writeErr(w, 400, "非法文件名")
		return
	}
	p := filepath.Join(m.instDir(id), "logs", file)
	f, err := os.Open(p) // 共享读，latest.log 被服务器占用时也能读
	if err != nil {
		writeErr(w, 404, "日志不存在")
		return
	}
	defer f.Close()

	var rd io.Reader = f
	if strings.HasSuffix(file, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			writeErr(w, 500, "解压失败: "+err.Error())
			return
		}
		defer gz.Close()
		rd = gz
	} else {
		if st, err := f.Stat(); err == nil && st.Size() > logTailBytes {
			f.Seek(st.Size()-logTailBytes, io.SeekStart)
		}
	}
	b, err := io.ReadAll(io.LimitReader(rd, logTailBytes))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(toUTF8(b)))
}
