package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ===== 导入本机已有服务器文件夹 =====

var verInJarRe = regexp.MustCompile(`(\d+\.\d+(?:\.\d+)?)`)

// detectServer looks for a launchable server entry (run.bat or a server jar)
// and tries to guess the loader type and MC version.
func detectServer(dir string) (jarFile, typ, version string, err error) {
	if _, e := os.Stat(filepath.Join(dir, "run.bat")); e == nil {
		typ = "forge"
		if ms, _ := filepath.Glob(filepath.Join(dir, "libraries", "net", "neoforged", "*")); len(ms) > 0 {
			typ = "neoforge"
		}
		return "run.bat", typ, "", nil
	}
	entries, e := os.ReadDir(dir)
	if e != nil {
		return "", "", "", fmt.Errorf("无法读取目录: %v", e)
	}
	best := ""
	for _, en := range entries {
		n := strings.ToLower(en.Name())
		if !strings.HasSuffix(n, ".jar") || strings.Contains(n, "installer") {
			continue
		}
		// 优先 paper/purpur/fabric/spigot 命名的 jar
		if strings.Contains(n, "paper") || strings.Contains(n, "purpur") ||
			strings.Contains(n, "fabric") || strings.Contains(n, "spigot") ||
			strings.Contains(n, "server") {
			best = en.Name()
			break
		}
		if best == "" {
			best = en.Name()
		}
	}
	if best == "" {
		return "", "", "", fmt.Errorf("目录里没有找到服务端 jar 或 run.bat")
	}
	low := strings.ToLower(best)
	switch {
	case strings.Contains(low, "paper"):
		typ = "paper"
	case strings.Contains(low, "fabric"):
		typ = "fabric"
	case strings.Contains(low, "forge"):
		typ = "forge"
	default:
		typ = "custom"
	}
	if mt := verInJarRe.FindStringSubmatch(best); mt != nil {
		version = mt[1]
	}
	return best, typ, version, nil
}

// copyDir recursively copies src into dst (dst created if missing).
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}

func (m *Manager) handleImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		Path     string `json:"path"`
		Port     int    `json:"port"`
		MemoryMB int    `json:"memoryMB"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" || body.Path == "" {
		writeErr(w, 400, "参数不完整（需要 name 和 path）")
		return
	}
	src, err := filepath.Abs(body.Path)
	if err != nil {
		writeErr(w, 400, "路径无效")
		return
	}
	info, err := os.Stat(src)
	if err != nil || !info.IsDir() {
		writeErr(w, 400, "文件夹不存在: "+src)
		return
	}
	// 禁止导入面板自己的数据目录
	dataAbs, _ := filepath.Abs(m.dataDir)
	if strings.HasPrefix(src+string(os.PathSeparator), dataAbs+string(os.PathSeparator)) {
		writeErr(w, 400, "不能导入面板数据目录")
		return
	}
	jarFile, typ, version, err := detectServer(src)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if body.Port <= 0 {
		body.Port = 25565
	}
	if body.MemoryMB <= 0 {
		body.MemoryMB = 2048
	}

	in := &Instance{
		ID:        newID(),
		Name:      body.Name,
		Type:      typ,
		Version:   version,
		Port:      body.Port,
		MemoryMB:  body.MemoryMB,
		JarFile:   jarFile,
		CreatedAt: time.Now(),
	}
	m.mu.Lock()
	m.insts[in.ID] = in
	rs := m.getRT(in.ID)
	rs.status = "downloading"
	rs.console.ClearProgress()
	m.save()
	m.mu.Unlock()

	m.addActivity("blue", fmt.Sprintf("正在导入已有服务器 <b>%s</b>", in.Name))
	go func() {
		hub := rs.console
		hub.Broadcast("[MCS] 正在复制服务器文件（大存档可能需要几分钟）...")
		err := copyDir(src, m.instDir(in.ID))
		m.mu.Lock()
		if err != nil {
			rs.status = "error"
			rs.errMsg = "复制文件失败: " + err.Error()
			m.mu.Unlock()
			hub.Broadcast("[MCS] 导入失败: " + err.Error())
			m.addActivity("orange", fmt.Sprintf("<b>%s</b> 导入失败", in.Name))
			return
		}
		rs.status = "stopped"
		m.mu.Unlock()
		// 确保 eula 已签，端口按导入设置
		os.WriteFile(filepath.Join(m.instDir(in.ID), "eula.txt"), []byte("eula=true\n"), 0644)
		hub.Broadcast("[MCS] 导入完成，可以启动了！原文件夹未被修改。")
		m.addActivity("green", fmt.Sprintf("已导入服务器 <b>%s</b>（%s %s）", in.Name, typ, version))
		m.notify("服务器导入完成", fmt.Sprintf("已有服务器「%s」（%s %s）已导入面板，随时可以启动。原文件夹未修改：%s", in.Name, typ, version, src))
	}()
	writeJSON(w, 201, m.snapshotLocked(in))
}
