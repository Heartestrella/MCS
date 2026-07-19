package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// ===== 已装模组/插件管理 =====

type installedFile struct {
	Name     string `json:"name"` // 文件名（含 .disabled 后缀则为禁用态）
	Dir      string `json:"dir"`  // plugins / mods / world/datapacks
	Size     int64  `json:"size"`
	Disabled bool   `json:"disabled"`
}

var modDirs = []string{"plugins", "mods", filepath.Join("world", "datapacks")}

// renameWithRetry renames a mod file, retrying briefly on Windows sharing
// violations（服务器刚停、java.exe 尚未完全退出时文件仍被锁住几秒）。
func renameWithRetry(oldPath, newPath string) error {
	var err error
	for i := 0; i < 10; i++ {
		if err = os.Rename(oldPath, newPath); err == nil {
			return nil
		}
		if !isSharingViolation(err) {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("文件被服务器进程占用（可能有残留的 java.exe 还没退出）。请确认服务器已完全停止后重试；如果反复出现，在任务管理器里结束残留的 java.exe")
}

func isSharingViolation(err error) bool {
	var errno syscall.Errno
	// ERROR_SHARING_VIOLATION(32) / ERROR_LOCK_VIOLATION(33)
	return errors.As(err, &errno) && (errno == 32 || errno == 33)
}

func (m *Manager) handleInstalledList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	_, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	out := []installedFile{}
	base := m.instDir(id)
	for _, sub := range modDirs {
		entries, err := os.ReadDir(filepath.Join(base, sub))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			n := e.Name()
			low := strings.ToLower(n)
			isJar := strings.HasSuffix(low, ".jar") || strings.HasSuffix(low, ".jar.disabled")
			isZip := strings.HasSuffix(low, ".zip") || strings.HasSuffix(low, ".zip.disabled")
			if !isJar && !isZip {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			out = append(out, installedFile{
				Name:     n,
				Dir:      filepath.ToSlash(sub),
				Size:     info.Size(),
				Disabled: strings.HasSuffix(low, ".disabled"),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir < out[j].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, 200, out)
}

// safeInstalledPath validates dir+name against the known mod dirs.
func (m *Manager) safeInstalledPath(id, dir, name string) (string, error) {
	okDir := false
	for _, d := range modDirs {
		if filepath.ToSlash(d) == dir {
			okDir = true
			break
		}
	}
	if !okDir {
		return "", fmt.Errorf("非法目录")
	}
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", fmt.Errorf("非法文件名")
	}
	low := strings.ToLower(name)
	if !strings.HasSuffix(low, ".jar") && !strings.HasSuffix(low, ".jar.disabled") &&
		!strings.HasSuffix(low, ".zip") && !strings.HasSuffix(low, ".zip.disabled") {
		return "", fmt.Errorf("仅支持 jar/zip 文件")
	}
	return filepath.Join(m.instDir(id), filepath.FromSlash(dir), name), nil
}

// handleInstalledAction: action = disable / enable / delete
func (m *Manager) handleInstalledAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Action string `json:"action"`
		Dir    string `json:"dir"`
		Name   string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
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
	p, err := m.safeInstalledPath(id, body.Dir, body.Name)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}

	switch body.Action {
	case "disable":
		if strings.HasSuffix(strings.ToLower(p), ".disabled") {
			writeErr(w, 400, "已是禁用状态")
			return
		}
		err = renameWithRetry(p, p+".disabled")
	case "enable":
		if !strings.HasSuffix(strings.ToLower(p), ".disabled") {
			writeErr(w, 400, "已是启用状态")
			return
		}
		err = renameWithRetry(p, p[:len(p)-len(".disabled")])
	case "delete":
		err = os.Remove(p)
	default:
		writeErr(w, 400, "未知操作")
		return
	}
	if err != nil {
		if os.IsNotExist(err) {
			writeErr(w, 404, "文件不存在")
		} else {
			writeErr(w, 500, err.Error())
		}
		return
	}
	actLabel := map[string]string{"disable": "禁用", "enable": "启用", "delete": "删除"}[body.Action]
	m.addActivity("blue", fmt.Sprintf("<b>%s</b>：%s了 <b>%s</b>", instName, actLabel, body.Name))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleInstalledUpload saves an uploaded jar/zip into the instance's mod dir.
func (m *Manager) handleInstalledUpload(w http.ResponseWriter, r *http.Request) {
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
	r.Body = http.MaxBytesReader(w, r.Body, 512<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, 400, "上传失败: "+err.Error())
		return
	}
	dir := r.FormValue("dir")
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, 400, "缺少文件")
		return
	}
	defer file.Close()

	name := filepath.Base(hdr.Filename)
	p, err := m.safeInstalledPath(id, dir, name)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out, err := os.Create(p)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		os.Remove(p)
		writeErr(w, 500, "写入失败: "+err.Error())
		return
	}
	out.Close()
	m.addActivity("green", fmt.Sprintf("<b>%s</b>：上传了 <b>%s</b>（%s）", instName, name, dir))
	writeJSON(w, 200, map[string]bool{"ok": true})
}
