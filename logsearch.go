package main

import (
	"bufio"
	"compress/gzip"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ===== 跨日志文件搜索（含 .gz 历史日志） =====

type logHit struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

const logSearchMax = 500

func (m *Manager) handleLogSearch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	_, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" || len(q) > 200 {
		writeErr(w, 400, "请输入 1~200 字的关键词")
		return
	}
	qLower := strings.ToLower(q)

	logDir := filepath.Join(m.instDir(id), "logs")
	entries, _ := os.ReadDir(logDir)
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && (strings.HasSuffix(n, ".log") || strings.HasSuffix(n, ".log.gz")) {
			names = append(names, n)
		}
	}
	// 新文件优先（latest.log 最前，其余按名字倒序≈按日期倒序）
	sort.Slice(names, func(i, j int) bool {
		if names[i] == "latest.log" {
			return true
		}
		if names[j] == "latest.log" {
			return false
		}
		return names[i] > names[j]
	})

	hits := []logHit{}
	truncated := false
	for _, name := range names {
		if len(hits) >= logSearchMax {
			truncated = true
			break
		}
		f, err := os.Open(filepath.Join(logDir, name))
		if err != nil {
			continue
		}
		var rd io.Reader = f
		if strings.HasSuffix(name, ".gz") {
			gz, err := gzip.NewReader(f)
			if err != nil {
				f.Close()
				continue
			}
			rd = gz
		}
		sc := bufio.NewScanner(rd)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		ln := 0
		for sc.Scan() {
			ln++
			line := toUTF8(sc.Bytes())
			if strings.Contains(strings.ToLower(line), qLower) {
				hits = append(hits, logHit{File: name, Line: ln, Text: cleanLine(line)})
				if len(hits) >= logSearchMax {
					truncated = true
					break
				}
			}
		}
		f.Close()
	}
	writeJSON(w, 200, map[string]any{
		"hits":      hits,
		"truncated": truncated,
		"files":     len(names),
	})
}
