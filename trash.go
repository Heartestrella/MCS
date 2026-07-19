package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ===== 回收站 =====
// 删除世界不再直接抹掉，而是移入 data/trash/，7 天内可一键恢复。

const trashKeep = 7 * 24 * time.Hour

func (m *Manager) trashDir() string { return filepath.Join(m.dataDir, "trash") }

type trashMeta struct {
	Instance  Instance  `json:"instance"`
	DeletedAt time.Time `json:"deletedAt"`
}

// moveToTrash relocates the instance dir and writes metadata; returns trash entry name.
func (m *Manager) moveToTrash(in *Instance) (string, error) {
	os.MkdirAll(m.trashDir(), 0755)
	entry := fmt.Sprintf("%s_%s", in.ID, time.Now().Format("20060102-150405"))
	dest := filepath.Join(m.trashDir(), entry)
	if err := os.Rename(m.instDir(in.ID), dest); err != nil {
		return "", err
	}
	meta := trashMeta{Instance: *in, DeletedAt: time.Now()}
	b, _ := json.MarshalIndent(meta, "", "  ")
	os.WriteFile(filepath.Join(dest, ".mcs-trash.json"), b, 0644)
	return entry, nil
}

// cleanTrash removes entries older than trashKeep. Called on startup.
func (m *Manager) cleanTrash() {
	entries, _ := os.ReadDir(m.trashDir())
	cut := time.Now().Add(-trashKeep)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(m.trashDir(), e.Name())
		var meta trashMeta
		if b, err := os.ReadFile(filepath.Join(p, ".mcs-trash.json")); err == nil {
			if json.Unmarshal(b, &meta) == nil && meta.DeletedAt.Before(cut) {
				os.RemoveAll(p)
			}
		} else if info, err := e.Info(); err == nil && info.ModTime().Before(cut) {
			os.RemoveAll(p)
		}
	}
}

func safeTrashName(name string) bool {
	return name != "" && !strings.ContainsAny(name, `/\`) && !strings.Contains(name, "..")
}

func (m *Manager) handleTrashList(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Name      string    `json:"name"`
		InstName  string    `json:"instName"`
		Type      string    `json:"type"`
		Version   string    `json:"version"`
		DeletedAt time.Time `json:"deletedAt"`
		Size      int64     `json:"size"`
	}
	out := []item{}
	entries, _ := os.ReadDir(m.trashDir())
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(m.trashDir(), e.Name())
		it := item{Name: e.Name()}
		if b, err := os.ReadFile(filepath.Join(p, ".mcs-trash.json")); err == nil {
			var meta trashMeta
			if json.Unmarshal(b, &meta) == nil {
				it.InstName = meta.Instance.Name
				it.Type = meta.Instance.Type
				it.Version = meta.Instance.Version
				it.DeletedAt = meta.DeletedAt
			}
		}
		it.Size = dirSize(p)
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeletedAt.After(out[j].DeletedAt) })
	writeJSON(w, 200, out)
}

func (m *Manager) handleTrashRestore(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !safeTrashName(name) {
		writeErr(w, 400, "非法名称")
		return
	}
	p := filepath.Join(m.trashDir(), name)
	b, err := os.ReadFile(filepath.Join(p, ".mcs-trash.json"))
	if err != nil {
		writeErr(w, 404, "回收站条目不存在或缺少元数据")
		return
	}
	var meta trashMeta
	if err := json.Unmarshal(b, &meta); err != nil {
		writeErr(w, 500, "元数据损坏")
		return
	}
	in := meta.Instance
	in.Dir = "" // 自定义路径实例恢复到默认目录（回收站条目在面板数据盘内）

	m.mu.Lock()
	if _, exists := m.insts[in.ID]; exists {
		in.ID = newID() // 原 ID 被占（比如又建了新世界），换新 ID
	}
	portTaken := false
	for _, other := range m.insts {
		if other.Port == in.Port {
			portTaken = true
			break
		}
	}
	m.mu.Unlock()
	if portTaken {
		in.Port = m.freePort(in.Port)
	}

	if err := os.Rename(p, m.instDir(in.ID)); err != nil {
		writeErr(w, 500, "恢复失败: "+err.Error())
		return
	}
	os.Remove(filepath.Join(m.instDir(in.ID), ".mcs-trash.json"))

	in.Status = "stopped"
	in.AutoStart = false
	m.mu.Lock()
	m.insts[in.ID] = &in
	m.save()
	snap := m.snapshot(&in)
	m.mu.Unlock()
	m.addActivity("green", fmt.Sprintf("世界 <b>%s</b> 已从回收站恢复", in.Name))
	writeJSON(w, 200, snap)
}

func (m *Manager) handleTrashDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !safeTrashName(name) {
		writeErr(w, 400, "非法名称")
		return
	}
	if err := os.RemoveAll(filepath.Join(m.trashDir(), name)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
