package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ===== 玩家活动统计 =====
// 每分钟采样在线人数（保留 7 天）；join/leave 时累计每个玩家的游玩时长。

type statSample struct {
	T time.Time `json:"t"`
	N int       `json:"n"`
}

type playerStat struct {
	Name      string    `json:"name"`
	TotalMin  int       `json:"totalMin"`
	LastSeen  time.Time `json:"lastSeen"`
	FirstSeen time.Time `json:"firstSeen"`
	JoinCount int       `json:"joinCount"`
}

type instStats struct {
	Samples []statSample           `json:"samples"`
	Players map[string]*playerStat `json:"players"`
	// 运行时：当前在线玩家的本次进入时间
	online map[string]time.Time
	dirty  bool
}

var (
	statsMu  sync.Mutex
	statsMap = map[string]*instStats{}
)

func (m *Manager) statsPath(id string) string {
	return filepath.Join(m.dataDir, "stats", id+".json")
}

// getStats loads (or creates) stats for an instance; caller must hold statsMu.
func (m *Manager) getStats(id string) *instStats {
	st, ok := statsMap[id]
	if ok {
		return st
	}
	st = &instStats{Players: map[string]*playerStat{}, online: map[string]time.Time{}}
	if b, err := os.ReadFile(m.statsPath(id)); err == nil {
		json.Unmarshal(b, st)
		if st.Players == nil {
			st.Players = map[string]*playerStat{}
		}
		st.online = map[string]time.Time{}
	}
	statsMap[id] = st
	return st
}

func (m *Manager) saveStats(id string, st *instStats) {
	os.MkdirAll(filepath.Join(m.dataDir, "stats"), 0755)
	b, _ := json.Marshal(st)
	os.WriteFile(m.statsPath(id), b, 0644)
}

// statsOnJoin/statsOnLeave are called from pipeConsole.
func (m *Manager) statsOnJoin(id, player string) {
	statsMu.Lock()
	defer statsMu.Unlock()
	st := m.getStats(id)
	now := time.Now()
	st.online[player] = now
	p, ok := st.Players[player]
	if !ok {
		p = &playerStat{Name: player, FirstSeen: now}
		st.Players[player] = p
	}
	p.JoinCount++
	p.LastSeen = now
	st.dirty = true
}

func (m *Manager) statsOnLeave(id, player string) {
	statsMu.Lock()
	defer statsMu.Unlock()
	st := m.getStats(id)
	now := time.Now()
	if p, ok := st.Players[player]; ok {
		if joined, on := st.online[player]; on {
			p.TotalMin += int(now.Sub(joined).Minutes())
		}
		p.LastSeen = now
	}
	delete(st.online, player)
	st.dirty = true
}

// statsOnStop settles playtime for everyone still online when server stops.
func (m *Manager) statsOnStop(id string) {
	statsMu.Lock()
	defer statsMu.Unlock()
	st := m.getStats(id)
	now := time.Now()
	for player, joined := range st.online {
		if p, ok := st.Players[player]; ok {
			p.TotalMin += int(now.Sub(joined).Minutes())
			p.LastSeen = now
		}
	}
	st.online = map[string]time.Time{}
	st.dirty = true
	m.saveStats(id, st)
	st.dirty = false
}

const statsKeep = 7 * 24 * time.Hour

// startStatsSampler samples online counts every minute and persists dirty stats.
func (m *Manager) startStatsSampler() {
	go func() {
		for range time.Tick(60 * time.Second) {
			m.mu.Lock()
			type snap struct {
				id string
				n  int
			}
			var snaps []snap
			for id, rs := range m.rt {
				if rs.status == "running" {
					snaps = append(snaps, snap{id, len(rs.players)})
				}
			}
			m.mu.Unlock()

			now := time.Now()
			cut := now.Add(-statsKeep)
			statsMu.Lock()
			for _, s := range snaps {
				st := m.getStats(s.id)
				st.Samples = append(st.Samples, statSample{T: now, N: s.n})
				for len(st.Samples) > 0 && st.Samples[0].T.Before(cut) {
					st.Samples = st.Samples[1:]
				}
				st.dirty = true
			}
			// 每分钟把脏数据落盘（量小，无所谓）
			for id, st := range statsMap {
				if st.dirty {
					m.saveStats(id, st)
					st.dirty = false
				}
			}
			statsMu.Unlock()
		}
	}()
}

func (m *Manager) handlePlayerStats(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	_, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	statsMu.Lock()
	st := m.getStats(id)
	now := time.Now()

	samples := make([]statSample, len(st.Samples))
	copy(samples, st.Samples)

	players := make([]playerStat, 0, len(st.Players))
	for _, p := range st.Players {
		cp := *p
		// 在线中的玩家把当前这段也算进去
		if joined, on := st.online[cp.Name]; on {
			cp.TotalMin += int(now.Sub(joined).Minutes())
		}
		players = append(players, cp)
	}
	statsMu.Unlock()

	sort.Slice(players, func(i, j int) bool { return players[i].TotalMin > players[j].TotalMin })
	if len(players) > 50 {
		players = players[:50]
	}
	writeJSON(w, 200, map[string]any{"samples": samples, "players": players})
}
