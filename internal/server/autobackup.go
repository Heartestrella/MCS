package server

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

// ===== 自动备份调度 =====

type AutoBackupConfig struct {
	Enabled     bool           `json:"enabled"`
	IntervalMin int            `json:"intervalMin"`           // 备份间隔（分钟）
	Keep        int            `json:"keep"`                  // 每个世界保留的自动备份数量
	OnlyRunning bool           `json:"onlyRunning"`           // 仅备份运行中的世界
	CloudUpload bool           `json:"cloudUpload"`           // 备份后自动上传云盘
	KeepPerInst map[string]int `json:"keepPerInst,omitempty"` // 按实例覆盖保留份数
}

// keepFor returns the retention for an instance (per-instance override or global).
func (c AutoBackupConfig) keepFor(instID string) int {
	if k, ok := c.KeepPerInst[instID]; ok && k >= 1 {
		return k
	}
	return c.Keep
}

func (m *Manager) autoBackupConfigPath() string { return filepath.Join(m.dataDir, "autobackup.json") }

func (m *Manager) loadAutoBackup() AutoBackupConfig {
	cfg := AutoBackupConfig{IntervalMin: 60, Keep: 5, OnlyRunning: true}
	if b, err := os.ReadFile(m.autoBackupConfigPath()); err == nil {
		json.Unmarshal(b, &cfg)
	}
	if cfg.IntervalMin < 10 {
		cfg.IntervalMin = 10
	}
	if cfg.Keep < 1 {
		cfg.Keep = 1
	}
	return cfg
}

func (m *Manager) handleAutoBackupGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, m.loadAutoBackup())
}

func (m *Manager) handleAutoBackupSet(w http.ResponseWriter, r *http.Request) {
	var cfg AutoBackupConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if cfg.IntervalMin < 10 {
		cfg.IntervalMin = 10
	}
	if cfg.Keep < 1 {
		cfg.Keep = 1
	}
	for id, k := range cfg.KeepPerInst {
		if k < 1 {
			delete(cfg.KeepPerInst, id)
		}
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(m.autoBackupConfigPath(), b, 0644); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	m.addActivity("blue", fmt.Sprintf("自动备份配置已更新（每 %d 分钟，保留 %d 份）", cfg.IntervalMin, cfg.Keep))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// startAutoBackup runs the periodic backup loop; call once from NewManager.
func (m *Manager) startAutoBackup() {
	go func() {
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		var last time.Time
		for range tick.C {
			cfg := m.loadAutoBackup()
			if !cfg.Enabled {
				continue
			}
			if time.Since(last) < time.Duration(cfg.IntervalMin)*time.Minute {
				continue
			}
			last = time.Now()
			m.runAutoBackup(cfg)
		}
	}()
}

func (m *Manager) runAutoBackup(cfg AutoBackupConfig) {
	m.mu.Lock()
	ids := make([]string, 0, len(m.insts))
	for id := range m.insts {
		st := m.getRT(id).status
		if cfg.OnlyRunning && st != "running" {
			continue
		}
		if st == "downloading" { // busy creating/restoring
			continue
		}
		ids = append(ids, id)
	}
	m.mu.Unlock()

	for _, id := range ids {
		file, size, err := m.doBackup(id)
		if err != nil {
			m.addActivity("orange", "自动备份失败: "+err.Error())
			continue
		}
		m.pruneBackups(id, cfg.keepFor(id))
		if cfg.CloudUpload {
			if err := m.cloudUploadFile(file); err != nil {
				m.addActivity("orange", "自动备份上传云盘失败: "+err.Error())
			} else {
				m.addActivity("green", fmt.Sprintf("自动备份「%s」已上传云盘（%.1f MB）", file, float64(size)/1e6))
			}
		}
	}
}

// pruneBackups keeps only the newest `keep` backups of an instance.
func (m *Manager) pruneBackups(instID string, keep int) {
	entries, err := os.ReadDir(m.backupDir())
	if err != nil {
		return
	}
	type bk struct {
		name string
		mod  time.Time
	}
	var mine []bk
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".zip") {
			continue
		}
		parts := strings.Split(strings.TrimSuffix(e.Name(), ".zip"), "_")
		if len(parts) < 3 || parts[len(parts)-2] != instID {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		mine = append(mine, bk{e.Name(), info.ModTime()})
	}
	if len(mine) <= keep {
		return
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].mod.After(mine[j].mod) })
	for _, old := range mine[keep:] {
		os.Remove(filepath.Join(m.backupDir(), old.name))
	}
}

// cloudUploadFile pushes one local backup zip to the WebDAV folder (no HTTP layer).
func (m *Manager) cloudUploadFile(file string) error {
	cfg := m.loadWebDAV()
	if !cfg.Enabled || cfg.URL == "" {
		return fmt.Errorf("云盘未启用")
	}
	p, err := m.safeBackupPath(file)
	if err != nil {
		return err
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	davReq(cfg, "MKCOL", "", nil) // best-effort ensure dir
	resp, err := davReq(cfg, "PUT", filepath.Base(p), f)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
