package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ===== 备份浏览器：查看 zip 内容、恢复单个文件/目录 =====
// 整包恢复会覆盖全部进度；实际常见需求是「误删了一个文件/一块地图，只想找回那一部分」。

type zipEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size"`
}

// handleBackupBrowse lists entries directly under `path` inside a backup zip.
func (m *Manager) handleBackupBrowse(w http.ResponseWriter, r *http.Request) {
	p, err := m.safeBackupPath(r.PathValue("file"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	prefix := strings.Trim(strings.ReplaceAll(r.URL.Query().Get("path"), "\\", "/"), "/")
	if strings.Contains(prefix, "..") {
		writeErr(w, 400, "非法路径")
		return
	}
	if prefix != "" {
		prefix += "/"
	}

	zr, err := zip.OpenReader(p)
	if err != nil {
		writeErr(w, 500, "打开备份失败: "+err.Error())
		return
	}
	defer zr.Close()

	dirs := map[string]bool{}
	var files []zipEntry
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		if !strings.HasPrefix(name, prefix) || name == prefix {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			dirs[rest[:i]] = true
			continue
		}
		if f.FileInfo().IsDir() {
			dirs[strings.TrimSuffix(rest, "/")] = true
			continue
		}
		files = append(files, zipEntry{Name: rest, Size: int64(f.UncompressedSize64)})
	}

	out := make([]zipEntry, 0, len(dirs)+len(files))
	for d := range dirs {
		if d != "" {
			out = append(out, zipEntry{Name: d, IsDir: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	out = append(out, files...)
	writeJSON(w, 200, out)
}

// handleBackupExtract restores selected files/dirs from a backup into an instance.
func (m *Manager) handleBackupExtract(w http.ResponseWriter, r *http.Request) {
	p, err := m.safeBackupPath(r.PathValue("file"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var body struct {
		InstID string   `json:"instId"`
		Paths  []string `json:"paths"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Paths) == 0 {
		writeErr(w, 400, "参数不完整")
		return
	}
	m.mu.Lock()
	in, ok := m.insts[body.InstID]
	var name string
	var running bool
	if ok {
		name = in.Name
		rs := m.getRT(body.InstID)
		running = rs.status != "stopped" && rs.status != "error"
	}
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}

	// 归一化选择的前缀
	var wants []string
	touchesWorld := false
	for _, sel := range body.Paths {
		sel = strings.Trim(strings.ReplaceAll(sel, "\\", "/"), "/")
		if sel == "" || strings.Contains(sel, "..") {
			writeErr(w, 400, "非法路径: "+sel)
			return
		}
		if sel == "world" || strings.HasPrefix(sel, "world") {
			touchesWorld = true
		}
		wants = append(wants, sel)
	}
	if running && touchesWorld {
		writeErr(w, 409, "服务器运行中不能恢复世界文件（会损坏存档），请先停止服务器；恢复配置类文件不受限")
		return
	}

	zr, err := zip.OpenReader(p)
	if err != nil {
		writeErr(w, 500, "打开备份失败: "+err.Error())
		return
	}
	defer zr.Close()

	dir := m.instDir(body.InstID)
	dirAbs, _ := filepath.Abs(dir)
	restored := 0
	var firstErr error
	for _, f := range zr.File {
		zname := strings.Trim(strings.ReplaceAll(f.Name, "\\", "/"), "/")
		match := false
		for _, want := range wants {
			if zname == want || strings.HasPrefix(zname, want+"/") {
				match = true
				break
			}
		}
		if !match || f.FileInfo().IsDir() {
			continue
		}
		dest := filepath.Join(dir, filepath.FromSlash(zname))
		destAbs, _ := filepath.Abs(dest)
		if !strings.HasPrefix(destAbs, dirAbs+string(os.PathSeparator)) {
			continue // zip slip 防护
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			firstErr = err
			continue
		}
		rc, err := f.Open()
		if err != nil {
			firstErr = err
			continue
		}
		out, err := os.Create(dest)
		if err != nil {
			rc.Close()
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		_, err = io.Copy(out, rc)
		out.Close()
		rc.Close()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		restored++
	}
	if restored == 0 {
		msg := "备份里没有匹配的文件"
		if firstErr != nil {
			msg = "恢复失败: " + firstErr.Error()
		}
		writeErr(w, 500, msg)
		return
	}
	m.addActivity("blue", fmt.Sprintf("世界 <b>%s</b> 从备份局部恢复了 %d 个文件", name, restored))
	resp := map[string]any{"ok": true, "restored": restored}
	if firstErr != nil {
		resp["warning"] = fmt.Sprintf("部分文件失败（可能被占用）: %v", firstErr)
	}
	writeJSON(w, 200, resp)
}
