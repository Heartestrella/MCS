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
	"time"
)

// ===== 本地备份 =====

type BackupInfo struct {
	File      string    `json:"file"`
	InstID    string    `json:"instId"`
	InstName  string    `json:"instName"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"createdAt"`
}

func (m *Manager) backupDir() string { return filepath.Join(m.dataDir, "backups") }

// zipDir zips srcDir into destZip, skipping session locks and previous backups.
func zipDir(srcDir, destZip string) error {
	f, err := os.Create(destZip)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	defer zw.Close()

	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable (e.g. locked) entries
		}
		if info.IsDir() {
			return nil
		}
		name := strings.ToLower(info.Name())
		if name == "session.lock" || strings.HasSuffix(name, ".part") {
			return nil
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		hdr, _ := zip.FileInfoHeader(info)
		hdr.Name = filepath.ToSlash(rel)
		hdr.Method = zip.Deflate
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return nil // locked file (server running), skip
		}
		defer src.Close()
		_, err = io.Copy(w, src)
		return err
	})
}

// doBackup zips one instance into the backup dir; safe on a live server
// (temporarily disables autosave and flushes the world first).
func (m *Manager) doBackup(id string) (string, int64, error) {
	m.mu.Lock()
	in, ok := m.insts[id]
	var name string
	var running bool
	if ok {
		name = in.Name
		rs := m.getRT(id)
		running = rs.status == "running"
	}
	m.mu.Unlock()
	if !ok {
		return "", 0, fmt.Errorf("实例不存在")
	}

	// flush world to disk while backing up a live server
	if running {
		m.sendCommand(in, "save-off")
		m.sendCommand(in, "save-all flush")
		time.Sleep(2 * time.Second)
	}

	os.MkdirAll(m.backupDir(), 0755)
	stamp := time.Now().Format("20060102-150405")
	zipName := fmt.Sprintf("%s_%s_%s.zip", sanitize(name), id, stamp)
	dest := filepath.Join(m.backupDir(), zipName)

	err := zipDir(m.instDir(id), dest)

	if running {
		m.sendCommand(in, "save-on")
	}
	if err != nil {
		os.Remove(dest)
		return "", 0, err
	}
	st, _ := os.Stat(dest)
	var size int64
	if st != nil {
		size = st.Size()
	}
	m.addActivity("green", fmt.Sprintf("世界 <b>%s</b> 备份完成（%.1f MB）", name, float64(size)/1e6))
	return zipName, size, nil
}

func (m *Manager) handleBackupCreate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	zipName, size, err := m.doBackup(id)
	if err != nil {
		if err.Error() == "实例不存在" {
			writeErr(w, 404, err.Error())
		} else {
			writeErr(w, 500, "备份失败: "+err.Error())
		}
		return
	}
	m.mu.Lock()
	var name string
	if in, ok := m.insts[id]; ok {
		name = in.Name
	}
	m.mu.Unlock()
	m.notify("备份完成", fmt.Sprintf("世界「%s」已备份为 %s（%.1f MB）", name, zipName, float64(size)/1e6))
	writeJSON(w, 200, map[string]any{"ok": true, "file": zipName, "size": size})
}

func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if strings.ContainsRune(`\/:*?"<>|_`, r) || r == ' ' {
			out = append(out, '-')
		} else {
			out = append(out, r)
		}
	}
	return string(out)
}

func (m *Manager) handleBackupList(w http.ResponseWriter, r *http.Request) {
	entries, _ := os.ReadDir(m.backupDir())
	out := []BackupInfo{}
	m.mu.Lock()
	names := map[string]string{}
	for id, in := range m.insts {
		names[id] = in.Name
	}
	m.mu.Unlock()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".zip") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		bi := BackupInfo{File: e.Name(), Size: info.Size(), CreatedAt: info.ModTime()}
		// filename layout: <name>_<id>_<stamp>.zip
		parts := strings.Split(strings.TrimSuffix(e.Name(), ".zip"), "_")
		if len(parts) >= 3 {
			bi.InstID = parts[len(parts)-2]
			if n, ok := names[bi.InstID]; ok {
				bi.InstName = n
			} else {
				bi.InstName = parts[0] + "（已删除）"
			}
		}
		out = append(out, bi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	writeJSON(w, 200, out)
}

func (m *Manager) safeBackupPath(file string) (string, error) {
	if file == "" || strings.ContainsAny(file, `/\`) || strings.Contains(file, "..") || !strings.HasSuffix(file, ".zip") {
		return "", fmt.Errorf("非法文件名")
	}
	return filepath.Join(m.backupDir(), file), nil
}

func (m *Manager) handleBackupDelete(w http.ResponseWriter, r *http.Request) {
	p, err := m.safeBackupPath(r.PathValue("file"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := os.Remove(p); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (m *Manager) handleBackupDownload(w http.ResponseWriter, r *http.Request) {
	p, err := m.safeBackupPath(r.PathValue("file"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filepath.Base(p)+"\"")
	http.ServeFile(w, r, p)
}

// handleBackupRestore unpacks a backup zip over an instance dir (server must be stopped).
func (m *Manager) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	var body struct {
		File   string `json:"file"`
		InstID string `json:"instId"`
		NoSnap bool   `json:"noSnap"` // 跳过恢复前安全快照
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	p, err := m.safeBackupPath(body.File)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	m.mu.Lock()
	in, ok := m.insts[body.InstID]
	var name string
	if ok {
		name = in.Name
		rs := m.getRT(body.InstID)
		if rs.status != "stopped" && rs.status != "error" {
			m.mu.Unlock()
			writeErr(w, 409, "请先停止服务器再恢复备份")
			return
		}
		rs.status = "downloading" // reuse busy state to lock start button
		rs.console.ClearProgress()
	}
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}

	dir := m.instDir(body.InstID)

	// 恢复前安全快照：万一恢复错了还能回退
	var snapFile string
	if !body.NoSnap {
		if f, _, serr := m.doBackup(body.InstID); serr == nil {
			snapFile = f
		}
	}

	err = unzip(p, dir)

	m.mu.Lock()
	m.getRT(body.InstID).status = "stopped"
	m.mu.Unlock()

	if err != nil {
		writeErr(w, 500, "恢复失败: "+err.Error())
		return
	}
	if snapFile != "" {
		m.addActivity("blue", fmt.Sprintf("恢复前已自动快照当前状态：%s", snapFile))
	}
	m.addActivity("blue", fmt.Sprintf("世界 <b>%s</b> 已从备份「%s」恢复", name, body.File))
	m.notify("备份恢复完成", fmt.Sprintf("世界「%s」已从备份 %s 恢复。", name, body.File))
	writeJSON(w, 200, map[string]any{"ok": true, "snapshot": snapFile})
}

// ===== WebDAV 云盘（坚果云 / Nutstore / TeraCloud / 群晖 等）=====

type WebDAVConfig struct {
	Enabled  bool   `json:"enabled"`
	URL      string `json:"url"` // e.g. https://dav.jianguoyun.com/dav/mcs-backups/
	User     string `json:"user"`
	Password string `json:"password"`
}

func (m *Manager) webdavConfigPath() string { return filepath.Join(m.dataDir, "webdav.json") }

func (m *Manager) loadWebDAV() WebDAVConfig {
	var cfg WebDAVConfig
	if b, err := os.ReadFile(m.webdavConfigPath()); err == nil {
		json.Unmarshal(b, &cfg)
	}
	return cfg
}

var davClient = &http.Client{Timeout: 30 * time.Minute}

func davReq(cfg WebDAVConfig, method, name string, body io.Reader) (*http.Response, error) {
	base := strings.TrimSuffix(cfg.URL, "/")
	u := base
	if name != "" {
		u += "/" + url.PathEscape(name)
	}
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(cfg.User, cfg.Password)
	return davClient.Do(req)
}

func (m *Manager) handleWebDAVGet(w http.ResponseWriter, r *http.Request) {
	cfg := m.loadWebDAV()
	if cfg.Password != "" {
		cfg.Password = "********"
	}
	writeJSON(w, 200, cfg)
}

func (m *Manager) handleWebDAVSet(w http.ResponseWriter, r *http.Request) {
	var cfg WebDAVConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if cfg.Password == "" || cfg.Password == "********" {
		cfg.Password = m.loadWebDAV().Password
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(m.webdavConfigPath(), b, 0600); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (m *Manager) handleWebDAVTest(w http.ResponseWriter, r *http.Request) {
	cfg := m.loadWebDAV()
	if cfg.URL == "" {
		writeErr(w, 400, "未配置 WebDAV 地址")
		return
	}
	// MKCOL ensures the folder exists; 405 = already exists, both fine
	resp, err := davReq(cfg, "MKCOL", "", nil)
	if err != nil {
		writeErr(w, 502, "连接失败: "+err.Error())
		return
	}
	resp.Body.Close()
	if resp.StatusCode == 401 {
		writeErr(w, 502, "认证失败，请检查账号和应用密码")
		return
	}
	if resp.StatusCode >= 400 && resp.StatusCode != 405 {
		writeErr(w, 502, fmt.Sprintf("WebDAV 返回 HTTP %d", resp.StatusCode))
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleCloudUpload pushes a local backup zip to the WebDAV folder.
func (m *Manager) handleCloudUpload(w http.ResponseWriter, r *http.Request) {
	cfg := m.loadWebDAV()
	if !cfg.Enabled || cfg.URL == "" {
		writeErr(w, 400, "云盘未启用")
		return
	}
	p, err := m.safeBackupPath(r.PathValue("file"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	f, err := os.Open(p)
	if err != nil {
		writeErr(w, 404, "备份文件不存在")
		return
	}
	defer f.Close()
	st, _ := f.Stat()

	davReq(cfg, "MKCOL", "", nil) // best-effort ensure dir

	resp, err := davReq(cfg, "PUT", filepath.Base(p), f)
	if err != nil {
		writeErr(w, 502, "上传失败: "+err.Error())
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		writeErr(w, 502, fmt.Sprintf("上传失败: HTTP %d", resp.StatusCode))
		return
	}
	m.addActivity("green", fmt.Sprintf("备份「%s」已上传云盘（%.1f MB）", filepath.Base(p), float64(st.Size())/1e6))
	m.notify("云盘上传完成", fmt.Sprintf("备份 %s 已上传至云盘。", filepath.Base(p)))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleCloudList lists zips in the WebDAV folder via PROPFIND.
func (m *Manager) handleCloudList(w http.ResponseWriter, r *http.Request) {
	cfg := m.loadWebDAV()
	if !cfg.Enabled || cfg.URL == "" {
		writeJSON(w, 200, []any{})
		return
	}
	req, err := http.NewRequest("PROPFIND", strings.TrimSuffix(cfg.URL, "/")+"/", strings.NewReader(
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:getcontentlength/><d:getlastmodified/></d:prop></d:propfind>`))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	req.SetBasicAuth(cfg.User, cfg.Password)
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml")
	resp, err := davClient.Do(req)
	if err != nil {
		writeErr(w, 502, "云盘连接失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		writeErr(w, 502, fmt.Sprintf("云盘返回 HTTP %d", resp.StatusCode))
		return
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	// light-weight parse: pull href + size pairs out of the multistatus XML
	out := []cloudFile{}
	s := string(body)
	sep := "<D:response>"
	if !strings.Contains(s, sep) {
		sep = "<d:response>"
	}
	for _, chunk := range strings.Split(s, sep) {
		if cf, ok := parseCloudFile(chunk); ok {
			out = append(out, cf)
		}
	}
	writeJSON(w, 200, out)
}

type cloudFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

func parseCloudFile(chunk string) (cloudFile, bool) {
	lower := strings.ToLower(chunk)
	hs := strings.Index(lower, "<d:href>")
	he := strings.Index(lower, "</d:href>")
	if hs < 0 || he <= hs {
		return cloudFile{}, false
	}
	href := chunk[hs+8 : he]
	if dec, err := url.PathUnescape(href); err == nil {
		href = dec
	}
	name := filepath.Base(strings.TrimSuffix(href, "/"))
	if !strings.HasSuffix(strings.ToLower(name), ".zip") {
		return cloudFile{}, false
	}
	var size int64
	if ls := strings.Index(lower, "getcontentlength"); ls >= 0 {
		rest := chunk[ls:]
		if gt := strings.Index(rest, ">"); gt >= 0 {
			if numEnd := strings.Index(rest[gt:], "<"); numEnd > 0 {
				fmt.Sscanf(rest[gt+1:gt+numEnd], "%d", &size)
			}
		}
	}
	return cloudFile{name, size}, true
}

// handleCloudPull downloads a zip from WebDAV into the local backup dir.
func (m *Manager) handleCloudPull(w http.ResponseWriter, r *http.Request) {
	cfg := m.loadWebDAV()
	if !cfg.Enabled || cfg.URL == "" {
		writeErr(w, 400, "云盘未启用")
		return
	}
	file := r.PathValue("file")
	p, err := m.safeBackupPath(file)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	resp, err := davReq(cfg, "GET", file, nil)
	if err != nil {
		writeErr(w, 502, "下载失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		writeErr(w, 502, fmt.Sprintf("下载失败: HTTP %d", resp.StatusCode))
		return
	}
	os.MkdirAll(m.backupDir(), 0755)
	tmp := p + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	_, err = io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		writeErr(w, 500, "下载中断: "+err.Error())
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	m.addActivity("blue", fmt.Sprintf("已从云盘拉取备份「%s」", file))
	writeJSON(w, 200, map[string]bool{"ok": true})
}
