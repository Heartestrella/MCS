package main

import (
	"net"
	"net/http"
	"runtime"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
)

var panelStart = time.Now()

// lanIP returns the machine's primary LAN IPv4 (the one used for outbound traffic).
func lanIP() string {
	conn, err := net.Dial("udp", "223.5.5.5:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP.String()
	}
	return ""
}

// handleSystemStats returns realtime host metrics for the dashboard.
func (m *Manager) handleSystemStats(w http.ResponseWriter, r *http.Request) {
	cpuPct := 0.0
	if ps, err := cpu.Percent(0, false); err == nil && len(ps) > 0 {
		cpuPct = ps[0]
	}

	var memUsed, memTotal uint64
	var memPct float64
	if vm, err := mem.VirtualMemory(); err == nil {
		memUsed, memTotal, memPct = vm.Used, vm.Total, vm.UsedPercent
	}

	var diskUsed, diskTotal uint64
	var diskPct float64
	if du, err := disk.Usage(m.dataDir); err == nil {
		diskUsed, diskTotal, diskPct = du.Used, du.Total, du.UsedPercent
	}

	var hostUptime uint64
	if up, err := host.Uptime(); err == nil {
		hostUptime = up
	}

	m.mu.Lock()
	running, totalMem := 0, 0
	type instCPU struct {
		ID   string  `json:"id"`
		Name string  `json:"name"`
		CPU  float64 `json:"cpu"`
		Mem  int     `json:"mem"`
	}
	var instStats []instCPU
	for id := range m.insts {
		if rs, ok := m.rt[id]; ok && (rs.status == "running" || rs.status == "starting") {
			running++
			totalMem += m.insts[id].MemoryMB
			instStats = append(instStats, instCPU{ID: id, Name: m.insts[id].Name, CPU: rs.cpuPct, Mem: rs.memUsedMB})
		}
	}
	instCount := len(m.insts)
	m.mu.Unlock()

	writeJSON(w, 200, map[string]any{
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
		"cpuPercent":  cpuPct,
		"memUsed":     memUsed,
		"memTotal":    memTotal,
		"memPercent":  memPct,
		"diskUsed":    diskUsed,
		"diskTotal":   diskTotal,
		"diskPercent": diskPct,
		"hostUptime":  hostUptime,
		"panelUptime": int(time.Since(panelStart).Seconds()),
		"instances":   instCount,
		"running":     running,
		"allocatedMB": totalMem,
		"javaPath":    findJava(),
		"numCPU":      runtime.NumCPU(),
		"lanIP":       lanIP(),
		"instStats":   instStats,
	})
}
