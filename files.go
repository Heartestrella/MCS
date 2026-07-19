package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ===== 实例文件管理 =====

type fileEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size"`
}

// resolveInstPath validates a user-supplied relative path inside the instance dir.
func (m *Manager) resolveInstPath(id, rel string) (string, error) {
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "/")
	if rel == "" || rel == "." {
		return m.instDir(id), nil
	}
	return safeJoin(m.instDir(id), rel)
}

func (m *Manager) instExists(id string) bool {
	m.mu.Lock()
	_, ok := m.insts[id]
	m.mu.Unlock()
	return ok
}

func (m *Manager) handleFilesList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !m.instExists(id) {
		writeErr(w, 404, "实例不存在")
		return
	}
	p, err := m.resolveInstPath(id, r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		writeErr(w, 404, "目录不存在")
		return
	}
	out := []fileEntry{}
	for _, e := range entries {
		fe := fileEntry{Name: e.Name(), IsDir: e.IsDir()}
		if info, err := e.Info(); err == nil && !e.IsDir() {
			fe.Size = info.Size()
		}
		out = append(out, fe)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, 200, out)
}

func (m *Manager) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !m.instExists(id) {
		writeErr(w, 404, "实例不存在")
		return
	}
	p, err := m.resolveInstPath(id, r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		writeErr(w, 404, "文件不存在")
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(p)))
	http.ServeFile(w, r, p)
}

func (m *Manager) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !m.instExists(id) {
		writeErr(w, 404, "实例不存在")
		return
	}
	rel := r.URL.Query().Get("path")
	p, err := m.resolveInstPath(id, rel)
	if err != nil || p == m.instDir(id) {
		writeErr(w, 400, "非法路径")
		return
	}
	// 保护核心文件
	base := strings.ToLower(filepath.Base(p))
	if base == "server.properties" || base == "eula.txt" {
		writeErr(w, 400, "核心文件不允许删除")
		return
	}
	info, err := os.Stat(p)
	if err != nil {
		writeErr(w, 404, "文件不存在")
		return
	}
	if info.IsDir() {
		err = os.RemoveAll(p)
	} else {
		err = os.Remove(p)
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (m *Manager) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !m.instExists(id) {
		writeErr(w, 404, "实例不存在")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, 400, "上传失败: "+err.Error())
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, 400, "缺少文件")
		return
	}
	defer file.Close()
	dir, err := m.resolveInstPath(id, r.FormValue("path"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	name := filepath.Base(hdr.Filename)
	if name == "" || name == "." || strings.ContainsAny(name, `/\`) {
		writeErr(w, 400, "非法文件名")
		return
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	dest := filepath.Join(dir, name)
	out, err := os.Create(dest)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		os.Remove(dest)
		writeErr(w, 500, "写入失败: "+err.Error())
		return
	}
	out.Close()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ===== 文本文件在线编辑 =====

var editableExts = map[string]bool{
	".yml": true, ".yaml": true, ".json": true, ".properties": true,
	".toml": true, ".txt": true, ".conf": true, ".cfg": true, ".ini": true,
	".log": true, ".mcmeta": true, ".env": true, ".bat": true, ".sh": true,
}

func isEditable(p string) bool {
	return editableExts[strings.ToLower(filepath.Ext(p))]
}

const maxEditSize = 2 << 20 // 2MB

func (m *Manager) handleFileRead(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !m.instExists(id) {
		writeErr(w, 404, "实例不存在")
		return
	}
	p, err := m.resolveInstPath(id, r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !isEditable(p) {
		writeErr(w, 400, "该文件类型不支持在线编辑")
		return
	}
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		writeErr(w, 404, "文件不存在")
		return
	}
	if info.Size() > maxEditSize {
		writeErr(w, 400, "文件超过 2MB，请下载后编辑")
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"content": string(b)})
}

func (m *Manager) handleFileWrite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !m.instExists(id) {
		writeErr(w, 404, "实例不存在")
		return
	}
	var body struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxEditSize*2)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	p, err := m.resolveInstPath(id, body.Path)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !isEditable(p) {
		writeErr(w, 400, "该文件类型不支持在线编辑")
		return
	}
	if err := os.WriteFile(p, []byte(body.Content), 0644); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ===== 服务器图标（server-icon.png，64x64，前端已缩放） =====

var pngMagic = []byte{0x89, 'P', 'N', 'G'}

func (m *Manager) handleIconGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !m.instExists(id) {
		writeErr(w, 404, "实例不存在")
		return
	}
	p := filepath.Join(m.instDir(id), "server-icon.png")
	if _, err := os.Stat(p); err != nil {
		writeErr(w, 404, "未设置图标")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, p)
}

func (m *Manager) handleIconSet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	var instName string
	if ok {
		instName = in.Name
	}
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 64x64 PNG 远小于 1MB
	b, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, 400, "读取失败: "+err.Error())
		return
	}
	if len(b) < 8 || string(b[:4]) != string(pngMagic) {
		writeErr(w, 400, "必须是 PNG 图片")
		return
	}
	if err := os.WriteFile(filepath.Join(m.instDir(id), "server-icon.png"), b, 0644); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	m.addActivity("green", fmt.Sprintf("<b>%s</b> 更新了服务器图标", instName))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ===== 重命名 / 新建文件夹 / 解压 / 目录打包 =====

func (m *Manager) handleFileRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !m.instExists(id) {
		writeErr(w, 404, "实例不存在")
		return
	}
	var body struct {
		Path    string `json:"path"`
		NewName string `json:"newName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
		writeErr(w, 400, "参数不完整")
		return
	}
	nn := strings.TrimSpace(body.NewName)
	if nn == "" || strings.ContainsAny(nn, `/\:*?"<>|`) {
		writeErr(w, 400, "新名称不合法")
		return
	}
	p, err := m.resolveInstPath(id, body.Path)
	if err != nil || p == m.instDir(id) {
		writeErr(w, 400, "非法路径")
		return
	}
	dest := filepath.Join(filepath.Dir(p), nn)
	if _, err := os.Stat(dest); err == nil {
		writeErr(w, 409, "同名文件已存在")
		return
	}
	if err := os.Rename(p, dest); err != nil {
		writeErr(w, 500, "重命名失败（文件可能被占用）: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (m *Manager) handleFileMkdir(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !m.instExists(id) {
		writeErr(w, 404, "实例不存在")
		return
	}
	var body struct {
		Path string `json:"path"` // 相对路径含新文件夹名
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Path) == "" {
		writeErr(w, 400, "参数不完整")
		return
	}
	p, err := m.resolveInstPath(id, body.Path)
	if err != nil || p == m.instDir(id) {
		writeErr(w, 400, "非法路径")
		return
	}
	if err := os.MkdirAll(p, 0755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleFileUnzip extracts an archive into a same-named folder next to it.
func (m *Manager) handleFileUnzip(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !m.instExists(id) {
		writeErr(w, 404, "实例不存在")
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
		writeErr(w, 400, "参数不完整")
		return
	}
	p, err := m.resolveInstPath(id, body.Path)
	if err != nil {
		writeErr(w, 400, "非法路径")
		return
	}
	low := strings.ToLower(p)
	if !strings.HasSuffix(low, ".zip") && !strings.HasSuffix(low, ".mrpack") {
		writeErr(w, 400, "只支持解压 zip / mrpack")
		return
	}
	dest := strings.TrimSuffix(strings.TrimSuffix(p, filepath.Ext(p)), ".")
	if _, err := os.Stat(dest); err == nil {
		writeErr(w, 409, "目标文件夹已存在: "+filepath.Base(dest))
		return
	}
	if err := unzip(p, dest); err != nil { // unzip 自带 zip slip 防护
		os.RemoveAll(dest)
		writeErr(w, 500, "解压失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"dest": filepath.Base(dest)})
}

// handleDirZip streams a directory as a zip download.
func (m *Manager) handleDirZip(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !m.instExists(id) {
		writeErr(w, 404, "实例不存在")
		return
	}
	rel := r.URL.Query().Get("path")
	p, err := m.resolveInstPath(id, rel)
	if err != nil {
		writeErr(w, 400, "非法路径")
		return
	}
	info, err := os.Stat(p)
	if err != nil || !info.IsDir() {
		writeErr(w, 404, "目录不存在")
		return
	}
	zipName := filepath.Base(p)
	if p == m.instDir(id) {
		zipName = "server-files"
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename*=UTF-8''`+url.PathEscape(zipName+".zip"))
	zw := zip.NewWriter(w)
	defer zw.Close()
	filepath.Walk(p, func(fp string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		relp, err := filepath.Rel(p, fp)
		if err != nil {
			return nil
		}
		addFileToZip(zw, fp, filepath.ToSlash(relp))
		return nil
	})
}
