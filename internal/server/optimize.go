package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v3/mem"
)

// ===== 一键性能优化 =====
// ① Aikar flags：社区公认的 G1GC 调优参数（manager.launch 根据 OptimizedFlags 使用）
// ② 配置体检：分析 server.properties 与内存分配，给出可一键应用的建议。

// aikarFlags returns the tuned JVM args for the given heap size (MB).
// 参考 flags.sh (Aikar)；>=12GB 用大堆参数变体。
func aikarFlags(memMB int) []string {
	base := []string{
		fmt.Sprintf("-Xms%dM", memMB),
		fmt.Sprintf("-Xmx%dM", memMB),
		"-XX:+UseG1GC",
		"-XX:+ParallelRefProcEnabled",
		"-XX:MaxGCPauseMillis=200",
		"-XX:+UnlockExperimentalVMOptions",
		"-XX:+DisableExplicitGC",
		"-XX:+AlwaysPreTouch",
		"-XX:G1HeapWastePercent=5",
		"-XX:G1MixedGCCountTarget=4",
		"-XX:G1MixedGCLiveThresholdPercent=90",
		"-XX:G1RSetUpdatingPauseTimePercent=5",
		"-XX:SurvivorRatio=32",
		"-XX:+PerfDisableSharedMem",
		"-XX:MaxTenuringThreshold=1",
		"-Dusing.aikars.flags=https://mcflags.emc.gs",
		"-Daikars.new.flags=true",
	}
	if memMB >= 12288 {
		base = append(base,
			"-XX:G1NewSizePercent=40",
			"-XX:G1MaxNewSizePercent=50",
			"-XX:G1HeapRegionSize=16M",
			"-XX:G1ReservePercent=15",
			"-XX:InitiatingHeapOccupancyPercent=20")
	} else {
		base = append(base,
			"-XX:G1NewSizePercent=30",
			"-XX:G1MaxNewSizePercent=40",
			"-XX:G1HeapRegionSize=8M",
			"-XX:G1ReservePercent=20",
			"-XX:InitiatingHeapOccupancyPercent=15")
	}
	return base
}

type optSuggestion struct {
	Key     string `json:"key"` // server.properties 键；特殊: __memory __flags
	Label   string `json:"label"`
	Current string `json:"current"`
	Advice  string `json:"advice"` // 推荐值
	Reason  string `json:"reason"`
	Status  string `json:"status"` // good / suggest
}

// handleOptimizeGet analyzes config and returns suggestions.
func (m *Manager) handleOptimizeGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	props, _ := readProps(m.propsPath(id))
	if props == nil {
		props = map[string]string{}
	}
	get := func(k, def string) string {
		if v, ok := props[k]; ok && v != "" {
			return v
		}
		return def
	}
	atoi := func(s string, def int) int {
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
		return def
	}

	var out []optSuggestion
	sug := func(key, label, cur, adv, reason string) {
		st := "suggest"
		if cur == adv {
			st = "good"
		}
		out = append(out, optSuggestion{key, label, cur, adv, reason, st})
	}

	// 视距：10 以上明显吃 CPU；8 是流畅与体验的平衡点
	vd := atoi(get("view-distance", "10"), 10)
	adv := "8"
	if vd <= 8 {
		adv = strconv.Itoa(vd)
	}
	sug("view-distance", "视距", strconv.Itoa(vd), adv, "视距是服务器最大性能开销之一，8 区块肉眼几乎无差别但能省大量 CPU/带宽")

	// 模拟距离：生物/红石运算范围，6 足够生存玩法
	sd := atoi(get("simulation-distance", "10"), 10)
	adv = "6"
	if sd <= 6 {
		adv = strconv.Itoa(sd)
	}
	sug("simulation-distance", "模拟距离", strconv.Itoa(sd), adv, "决定生物 AI/作物生长的运算范围，6 区块不影响正常生存，卡顿大户")

	// network-compression-threshold：局域网/小服 512 更省 CPU
	nct := get("network-compression-threshold", "256")
	sug("network-compression-threshold", "网络压缩阈值", nct, "512", "本地/小服带宽充裕，调高阈值减少压缩 CPU 消耗")

	// 内存建议：不超过物理内存的一半，至少 2GB
	var memAdvice string
	if vm, err := mem.VirtualMemory(); err == nil {
		totalMB := int(vm.Total / 1024 / 1024)
		rec := totalMB / 2
		if rec > 8192 {
			rec = 8192
		}
		rec = rec / 1024 * 1024 // 取整 GB
		if rec < 2048 {
			rec = 2048
		}
		memAdvice = strconv.Itoa(rec)
		st := "good"
		if in.MemoryMB > totalMB*7/10 {
			st = "suggest"
			out = append(out, optSuggestion{"__memory", "最大内存", strconv.Itoa(in.MemoryMB), memAdvice,
				fmt.Sprintf("分配超过物理内存(%.0fGB)的 70%%，容易挤占系统导致整机卡顿", float64(totalMB)/1024), st})
		} else if in.MemoryMB < 2048 {
			out = append(out, optSuggestion{"__memory", "最大内存", strconv.Itoa(in.MemoryMB), "2048",
				"低于 2GB 现代版本容易频繁 GC 卡顿", "suggest"})
		}
	}

	// Aikar flags
	flagsSt := "suggest"
	if in.OptimizedFlags {
		flagsSt = "good"
	}
	if in.Type != "generic" {
		out = append(out, optSuggestion{"__flags", "优化 JVM 参数（Aikar flags）",
			map[bool]string{true: "已启用", false: "未启用"}[in.OptimizedFlags], "启用",
			"Paper 官方推荐的 G1GC 调优参数集，显著减少 GC 卡顿（重启生效）", flagsSt})
	}

	writeJSON(w, 200, map[string]any{"suggestions": out})
}

// handleOptimizeApply applies all suggested values in one click.
func (m *Manager) handleOptimizeApply(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	var body struct {
		Items []optSuggestion `json:"items"`
	}
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
	applied := 0
	touchedProps := false
	for _, it := range body.Items {
		switch it.Key {
		case "__memory":
			if mb, err := strconv.Atoi(it.Advice); err == nil && mb >= 512 && mb <= 65536 {
				m.mu.Lock()
				in.MemoryMB = mb
				m.save()
				m.mu.Unlock()
				applied++
			}
		case "__flags":
			m.mu.Lock()
			in.OptimizedFlags = true
			m.save()
			m.mu.Unlock()
			applied++
		default:
			k := strings.TrimSpace(it.Key)
			if k == "" || strings.ContainsAny(k, "=\n\r") || strings.ContainsAny(it.Advice, "\n\r") {
				continue
			}
			props[k] = it.Advice
			touchedProps = true
			applied++
		}
	}
	if touchedProps {
		var sb strings.Builder
		sb.WriteString("# Optimized by MCS " + time.Now().Format("2006-01-02 15:04:05") + "\n")
		for k, v := range props {
			sb.WriteString(k + "=" + v + "\n")
		}
		if err := os.WriteFile(m.propsPath(id), []byte(sb.String()), 0644); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	m.addActivity("green", fmt.Sprintf("<b>%s</b> 应用了 %d 项性能优化（重启生效）", in.Name, applied))
	writeJSON(w, 200, map[string]any{"applied": applied})
}
