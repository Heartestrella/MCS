package main

import (
	"io"
	"regexp"
	"strconv"
	"time"
)

// ===== TPS 实时监控 =====
// Paper/Purpur 系服务器运行时每 30s 静默发送 tps 指令，
// 拦截响应行解析 1m/5m/15m TPS，不广播到控制台避免刷屏。

// Paper:  "TPS from last 1m, 5m, 15m: 20.0, 20.0, 20.0"
// Purpur: "TPS from last 5s, 1m, 5m, 15m: 20.0, 20.0, 20.0, 20.0"（可能带 * 前缀；取最后三个 = 1m/5m/15m）
var reTPS = regexp.MustCompile(`TPS from last (?:5s, )?1m, 5m, 15m:(?:\s*\*?[\d.]+,)?\s*\*?([\d.]+),\s*\*?([\d.]+),\s*\*?([\d.]+)\s*$`)

func isPaperLike(typ string) bool {
	return typ == "paper" || typ == "purpur"
}

// startTPSSampler launches the global 30s sampling loop. Called from NewManager.
func (m *Manager) startTPSSampler() {
	go func() {
		for range time.Tick(30 * time.Second) {
			type target struct {
				rs    *runtimeState
				stdin io.WriteCloser
			}
			m.mu.Lock()
			var ts []target
			for id, rs := range m.rt {
				in := m.insts[id]
				if in == nil || !isPaperLike(in.Type) {
					continue
				}
				if rs.status == "running" && rs.stdin != nil {
					ts = append(ts, target{rs, rs.stdin})
				}
			}
			m.mu.Unlock()
			for _, t := range ts {
				m.mu.Lock()
				t.rs.tpsSilent = true
				m.mu.Unlock()
				io.WriteString(t.stdin, "tps\r\n")
			}
		}
	}()
}

// tpsOnLine intercepts tps command responses. Returns true when the line was a
// silent panel-initiated response that should NOT be broadcast to the console.
func (m *Manager) tpsOnLine(rs *runtimeState, line string) bool {
	mt := reTPS.FindStringSubmatch(line)
	if mt == nil {
		return false
	}
	t1, _ := strconv.ParseFloat(mt[1], 64)
	t5, _ := strconv.ParseFloat(mt[2], 64)
	t15, _ := strconv.ParseFloat(mt[3], 64)
	m.mu.Lock()
	rs.tps = [3]float64{t1, t5, t15}
	rs.tpsAt = time.Now()
	silent := rs.tpsSilent
	rs.tpsSilent = false
	m.mu.Unlock()
	return silent
}
