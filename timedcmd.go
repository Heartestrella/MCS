package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// ===== 实例定时指令（定时公告 / 自动保存等） =====

type TimedCmd struct {
	ID          string `json:"id"`
	IntervalMin int    `json:"intervalMin"`
	Command     string `json:"command"`
	Enabled     bool   `json:"enabled"`
}

// timedCmds are stored per instance: map[instID][]TimedCmd
func (m *Manager) timedCmdsPath() string { return filepath.Join(m.dataDir, "timedcmds.json") }

func (m *Manager) loadTimedCmds() map[string][]TimedCmd {
	out := map[string][]TimedCmd{}
	if b, err := os.ReadFile(m.timedCmdsPath()); err == nil {
		json.Unmarshal(b, &out)
	}
	return out
}

func (m *Manager) saveTimedCmds(all map[string][]TimedCmd) error {
	b, _ := json.MarshalIndent(all, "", "  ")
	return os.WriteFile(m.timedCmdsPath(), b, 0644)
}

func (m *Manager) handleTimedCmdsGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !m.instExists(id) {
		writeErr(w, 404, "实例不存在")
		return
	}
	cmds := m.loadTimedCmds()[id]
	if cmds == nil {
		cmds = []TimedCmd{}
	}
	writeJSON(w, 200, cmds)
}

func (m *Manager) handleTimedCmdsSet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !m.instExists(id) {
		writeErr(w, 404, "实例不存在")
		return
	}
	var cmds []TimedCmd
	if err := json.NewDecoder(r.Body).Decode(&cmds); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if len(cmds) > 20 {
		writeErr(w, 400, "最多 20 条定时指令")
		return
	}
	for i := range cmds {
		if cmds[i].IntervalMin < 1 {
			cmds[i].IntervalMin = 1
		}
		if cmds[i].ID == "" {
			cmds[i].ID = newID()
		}
	}
	all := m.loadTimedCmds()
	all[id] = cmds
	if err := m.saveTimedCmds(all); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, cmds)
}

// startTimedCmds ticks every minute and fires due commands on running instances.
func (m *Manager) startTimedCmds() {
	go func() {
		tick := 0
		for range time.Tick(time.Minute) {
			tick++
			all := m.loadTimedCmds()
			for instID, cmds := range all {
				m.mu.Lock()
				in, ok := m.insts[instID]
				var running bool
				if ok {
					rs := m.getRT(instID)
					running = rs.status == "running"
				}
				m.mu.Unlock()
				if !ok || !running {
					continue
				}
				for _, c := range cmds {
					if !c.Enabled || c.Command == "" || tick%c.IntervalMin != 0 {
						continue
					}
					if err := m.sendCommand(in, c.Command); err == nil {
						m.getRT(instID).console.Broadcast(fmt.Sprintf("[MCS] 定时指令已执行: %s", c.Command))
					}
				}
			}
		}
	}()
}
