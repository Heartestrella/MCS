package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ===== 世界管理：查看维度大小 / 重置世界或单个维度 / 换种子 =====

// dirSize sums the size of all files under a path.
func dirSize(root string) int64 {
	var total int64
	filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// worldDirs maps a reset target to instance-relative dirs.
// Paper 把三个维度分开；原版/Fabric/Forge 是 world/DIM-1、world/DIM1 子目录。
func worldTargets(dir string) map[string][]string {
	t := map[string][]string{
		"all": {"world", "world_nether", "world_the_end"},
	}
	if _, err := os.Stat(filepath.Join(dir, "world_nether")); err == nil {
		t["nether"] = []string{"world_nether"}
	} else {
		t["nether"] = []string{filepath.Join("world", "DIM-1")}
	}
	if _, err := os.Stat(filepath.Join(dir, "world_the_end")); err == nil {
		t["end"] = []string{"world_the_end"}
	} else {
		t["end"] = []string{filepath.Join("world", "DIM1")}
	}
	return t
}

func (m *Manager) handleWorldGet(w http.ResponseWriter, r *http.Request) {
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
	seed := ""
	if props != nil {
		seed = props["level-seed"]
	}

	type dim struct {
		Key   string `json:"key"`
		Label string `json:"label"`
		Size  int64  `json:"size"`
		Found bool   `json:"found"`
	}
	dims := []dim{}
	overworld := filepath.Join(dir, "world")
	if st, err := os.Stat(overworld); err == nil && st.IsDir() {
		sz := dirSize(overworld)
		// Paper: world 不含 nether/end；原版: 含 DIM-1/DIM1，扣掉单独展示
		nether := filepath.Join(dir, "world_nether")
		end := filepath.Join(dir, "world_the_end")
		if _, err := os.Stat(nether); err == nil {
			dims = append(dims,
				dim{"all", "主世界", sz, true},
				dim{"nether", "地狱", dirSize(nether), true},
				dim{"end", "末地", dirSize(end), true})
		} else {
			d1 := dirSize(filepath.Join(overworld, "DIM-1"))
			d2 := dirSize(filepath.Join(overworld, "DIM1"))
			dims = append(dims,
				dim{"all", "主世界", sz - d1 - d2, true},
				dim{"nether", "地狱", d1, d1 > 0},
				dim{"end", "末地", d2, d2 > 0})
		}
	}
	writeJSON(w, 200, map[string]any{"seed": seed, "dims": dims})
}

// handleWorldReset deletes world dirs (optionally backing up first) and can set a new seed.
func (m *Manager) handleWorldReset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Target string `json:"target"` // all / nether / end
		Seed   string `json:"seed"`
		Backup bool   `json:"backup"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求格式错误")
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
		writeErr(w, 409, "请先停止服务器再重置世界")
		return
	}
	rs.status = "downloading" // 借用 busy 状态防止重置期间启动
	rs.console.ClearProgress()
	name := in.Name
	m.mu.Unlock()

	release := func() {
		m.mu.Lock()
		rs.status = "stopped"
		m.mu.Unlock()
	}

	dir := m.instDir(id)
	targets, ok2 := worldTargets(dir)[body.Target]
	if !ok2 {
		release()
		writeErr(w, 400, "target 应为 all / nether / end")
		return
	}

	if body.Backup {
		if _, _, err := m.doBackup(id); err != nil {
			release()
			writeErr(w, 500, "重置前备份失败，已取消: "+err.Error())
			return
		}
	}

	removed := 0
	for _, rel := range targets {
		p := filepath.Join(dir, rel)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			release()
			writeErr(w, 500, fmt.Sprintf("删除 %s 失败: %v（可能有程序占用文件）", rel, err))
			return
		}
		removed++
	}

	// 换种子（仅整世界重置时有意义）
	if body.Target == "all" {
		seed := strings.TrimSpace(body.Seed)
		props, err := readProps(m.propsPath(id))
		if err == nil && props != nil {
			props["level-seed"] = seed
			var sb strings.Builder
			sb.WriteString("# Edited by MCS World Reset\n")
			for k, v := range props {
				sb.WriteString(k + "=" + v + "\n")
			}
			os.WriteFile(m.propsPath(id), []byte(sb.String()), 0644)
		}
	}

	release()
	label := map[string]string{"all": "整个世界", "nether": "地狱", "end": "末地"}[body.Target]
	m.addActivity("orange", fmt.Sprintf("<b>%s</b> 重置了%s", name, label))
	m.notify("世界已重置", fmt.Sprintf("世界「%s」的%s已重置（删除 %d 个目录%s）。下次启动会重新生成。",
		name, label, removed, map[bool]string{true: "，已提前备份", false: ""}[body.Backup]))
	writeJSON(w, 200, map[string]any{"ok": true, "removed": removed})
}
