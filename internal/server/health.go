package server

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// ===== 卡顿监测 =====
// 捕捉服务器日志里的 "Can't keep up!"（tick 落后）事件，
// 结合进程 CPU/内存历史曲线，给出健康评级；卡顿频繁时主动提醒。

type lagEvent struct {
	T        time.Time `json:"t"`
	BehindMs int       `json:"behindMs"`
	Ticks    int       `json:"ticks"`
}

type perfPoint struct {
	T   time.Time `json:"t"`
	CPU float64   `json:"cpu"`
	Mem int       `json:"mem"` // MB
}

type healthState struct {
	lags     []lagEvent  // 最近 200 条
	perf     []perfPoint // 15s 粒度，保留 1h（240 点）
	lastMail time.Time
}

var (
	healthMu  sync.Mutex
	healthMap = map[string]*healthState{}
)

func getHealth(id string) *healthState {
	hs, ok := healthMap[id]
	if !ok {
		hs = &healthState{}
		healthMap[id] = hs
	}
	return hs
}

// reLag matches: Can't keep up! Is the server overloaded? Running 2500ms or 50 ticks behind
var reLag = regexp.MustCompile(`Can't keep up!.*Running (\d+)ms(?: or (\d+) ticks)? behind`)

// healthOnLine is called from pipeConsole for every console line.
func (m *Manager) healthOnLine(in *Instance, line string) {
	mt := reLag.FindStringSubmatch(line)
	if mt == nil {
		return
	}
	behind, _ := strconv.Atoi(mt[1])
	ticks := 0
	if mt[2] != "" {
		ticks, _ = strconv.Atoi(mt[2])
	}
	now := time.Now()

	healthMu.Lock()
	hs := getHealth(in.ID)
	hs.lags = append(hs.lags, lagEvent{T: now, BehindMs: behind, Ticks: ticks})
	if len(hs.lags) > 200 {
		hs.lags = hs.lags[len(hs.lags)-200:]
	}
	// 最近 5 分钟卡顿次数
	cut := now.Add(-5 * time.Minute)
	recent := 0
	for _, e := range hs.lags {
		if e.T.After(cut) {
			recent++
		}
	}
	shouldAlert := recent >= 3 && now.Sub(hs.lastMail) > 30*time.Minute
	if shouldAlert {
		hs.lastMail = now
	}
	healthMu.Unlock()

	if shouldAlert {
		m.addActivity("orange", fmt.Sprintf("<b>%s</b> 5 分钟内卡顿 %d 次，建议做性能体检", in.Name, recent))
		m.notify("服务器卡顿告警",
			fmt.Sprintf("世界「%s」5 分钟内出现 %d 次 tick 卡顿（最近一次落后 %dms）。\n建议：管理中心 → 服务器配置 → 性能体检，一键应用优化；或减少模组/降低视距。", in.Name, recent, behind))
	}
}

// healthRecordPerf is called from sampleProcStats (every 3s); downsample to 15s.
func (m *Manager) healthRecordPerf(id string, cpu float64, memMB int) {
	now := time.Now()
	healthMu.Lock()
	defer healthMu.Unlock()
	hs := getHealth(id)
	if n := len(hs.perf); n > 0 && now.Sub(hs.perf[n-1].T) < 15*time.Second {
		return
	}
	hs.perf = append(hs.perf, perfPoint{T: now, CPU: cpu, Mem: memMB})
	if len(hs.perf) > 240 {
		hs.perf = hs.perf[len(hs.perf)-240:]
	}
}

func (m *Manager) handleHealth(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	var memMax int
	var status string
	var tps [3]float64
	var tpsAt time.Time
	if ok {
		memMax = in.MemoryMB
		rs := m.getRT(id)
		status = rs.status
		tps = rs.tps
		tpsAt = rs.tpsAt
	}
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}

	healthMu.Lock()
	hs := getHealth(id)
	perf := make([]perfPoint, len(hs.perf))
	copy(perf, hs.perf)
	cut := time.Now().Add(-time.Hour)
	var lags []lagEvent
	for _, e := range hs.lags {
		if e.T.After(cut) {
			lags = append(lags, e)
		}
	}
	healthMu.Unlock()

	// 简单评级：最近 10 分钟卡顿次数
	cut10 := time.Now().Add(-10 * time.Minute)
	recent := 0
	for _, e := range lags {
		if e.T.After(cut10) {
			recent++
		}
	}
	grade := "good"
	label := "运行流畅"
	switch {
	case status != "running":
		grade, label = "idle", "未在运行"
	case recent >= 5:
		grade, label = "bad", "卡顿严重，建议立即优化"
	case recent >= 1:
		grade, label = "warn", "偶有卡顿"
	}

	out := map[string]any{
		"grade":   grade,
		"label":   label,
		"memMax":  memMax,
		"perf":    perf,
		"lags":    lags,
		"lagsLen": len(lags),
	}
	if status == "running" && time.Since(tpsAt) < 2*time.Minute {
		out["tps"] = tps
	}
	writeJSON(w, 200, out)
}
