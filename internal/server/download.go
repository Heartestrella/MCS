package server

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// dlClient 禁用 HTTP/2：Go 默认把同一域名的并发请求复用到一条 HTTP/2 连接上，
// 并发下载 mod 时 16 路全挤在一条 TCP 连接里，被 CDN 按单连接限速后等于串行。
// 改用 HTTP/1.1 让每个并发请求各占一条连接，才能真正跑满带宽。
var dlClient = &http.Client{
	Timeout: 30 * time.Minute, // 大文件走代理可能很慢，超时放宽
	Transport: &http.Transport{
		Proxy:               dlProxyFunc,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	},
}

// stallWindow: 连续这么久收不到任何数据就判定断流、中止传输，
// 让上层尽快换源重试，而不是在被 CDN 掐死的连接上干等几分钟。
const stallWindow = 30 * time.Second

// stallBody wraps a response body and force-closes it when no data arrives
// within stallWindow, failing the pending Read immediately.
type stallBody struct {
	rc      io.ReadCloser
	last    atomic.Int64 // UnixNano of last data
	stalled atomic.Bool
	once    sync.Once
	done    chan struct{}
}

func withStallAbort(rc io.ReadCloser) io.ReadCloser {
	s := &stallBody{rc: rc, done: make(chan struct{})}
	s.last.Store(time.Now().UnixNano())
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.done:
				return
			case <-t.C:
				if time.Since(time.Unix(0, s.last.Load())) > stallWindow {
					s.stalled.Store(true)
					s.rc.Close() // 让阻塞中的 Read 立即报错
					return
				}
			}
		}
	}()
	return s
}

func (s *stallBody) Read(p []byte) (int, error) {
	n, err := s.rc.Read(p)
	if n > 0 {
		s.last.Store(time.Now().UnixNano())
	}
	if err != nil && s.stalled.Load() {
		err = fmt.Errorf("连续 %d 秒未收到数据，判定断流中止", int(stallWindow.Seconds()))
	}
	return n, err
}

func (s *stallBody) Close() error {
	s.once.Do(func() { close(s.done) })
	return s.rc.Close()
}

const fillAPI = "https://fill.papermc.io/v3/projects/paper"

// copyWithProgress copies resp body to f, updating hub progress and broadcasting
// console lines every ~10%. label is shown on the instance card progress bar;
// label 为空表示静默模式：只累计字节数用于测速，不刷进度条/控制台
// （并发下载 mod 时进度条按文件数显示，单文件进度会互相覆盖）。
func copyWithProgress(f *os.File, body io.Reader, total int64, hub *ConsoleHub, label string) (int64, error) {
	var written int64
	buf := make([]byte, 256*1024)
	lastPct := -1
	lastUpd := time.Now()
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return written, werr
			}
			written += int64(n)
			if hub != nil {
				hub.AddBytes(int64(n))
			}
			if hub != nil && label != "" && time.Since(lastUpd) > 200*time.Millisecond {
				lastUpd = time.Now()
				hub.SetProgress(label, written, total)
			}
			if hub != nil && label != "" && total > 0 {
				pct := int(written * 100 / total)
				if pct/10 > lastPct/10 {
					lastPct = pct
					hub.Broadcast(fmt.Sprintf("[MCS] %s %d%% (%.1f/%.1f MB)", label, pct, float64(written)/1e6, float64(total)/1e6))
				}
			}
		}
		if rerr == io.EOF {
			if hub != nil && label != "" {
				hub.SetProgress(label, written, total)
			}
			return written, nil
		}
		if rerr != nil {
			return written, rerr
		}
	}
}

// downloadWithProgress fetches url into destPath (.part + rename), reporting progress to hub.
func downloadWithProgress(rawURL, destPath string, hub *ConsoleHub, label string) error {
	req, _ := http.NewRequest("GET", rawURL, nil)
	req.Header.Set("User-Agent", "mcs-panel/1.0 (github.com/mcs-panel)")
	resp, err := dlClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	tmp := destPath + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	body := withStallAbort(resp.Body)
	defer body.Close()
	if _, err := copyWithProgress(f, body, resp.ContentLength, hub, label); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	return os.Rename(tmp, destPath)
}

func fetchJSON(url string, v any) error {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "mcs-panel/1.0 (github.com/mcs-panel)")
	resp, err := dlClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func isStableVersion(v string) bool {
	for _, part := range strings.Split(v, ".") {
		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}
	return true
}

func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		na, nb := 0, 0
		if i < len(pa) {
			na, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			nb, _ = strconv.Atoi(pb[i])
		}
		if na != nb {
			return na < nb
		}
	}
	return false
}

func paperVersions() ([]string, error) {
	var data struct {
		Versions map[string][]string `json:"versions"`
	}
	if err := fetchJSON(fillAPI, &data); err != nil {
		return nil, err
	}
	var out []string
	for _, list := range data.Versions {
		for _, v := range list {
			if isStableVersion(v) {
				out = append(out, v)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return versionLess(out[j], out[i]) })
	return out, nil
}

// downloadPaper fetches the latest build jar for a version into dir, reporting progress lines to hub.
func downloadPaper(version, dir string, hub *ConsoleHub) (string, error) {
	hub.StepRun(stepCoreInfo)
	var build struct {
		ID        int    `json:"id"`
		Channel   string `json:"channel"`
		Downloads map[string]struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
			URL  string `json:"url"`
		} `json:"downloads"`
	}
	if err := fetchJSON(fmt.Sprintf("%s/versions/%s/builds/latest", fillAPI, version), &build); err != nil {
		hub.StepFail(stepCoreInfo)
		return "", fmt.Errorf("获取构建信息失败: %w", err)
	}
	dl, ok := build.Downloads["server:default"]
	if !ok {
		for _, d := range build.Downloads {
			dl, ok = d, true
			break
		}
	}
	if !ok || dl.URL == "" {
		hub.StepFail(stepCoreInfo)
		return "", fmt.Errorf("版本 %s 没有可下载的服务端", version)
	}
	hub.StepDone(stepCoreInfo)

	hub.Broadcast(fmt.Sprintf("[MCS] 正在下载 Paper %s build %d (%.1f MB)...", version, build.ID, float64(dl.Size)/1e6))

	req, _ := http.NewRequest("GET", dl.URL, nil)
	req.Header.Set("User-Agent", "mcs-panel/1.0 (github.com/mcs-panel)")
	resp, err := dlClient.Do(req)
	if err != nil {
		hub.StepFail(stepCoreJar)
		return "", fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		hub.StepFail(stepCoreJar)
		return "", fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}

	tmp := filepath.Join(dir, dl.Name+".part")
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	total := resp.ContentLength
	if total <= 0 {
		total = dl.Size
	}
	if _, err := copyWithProgress(f, resp.Body, total, hub, stepCoreJar); err != nil {
		f.Close()
		os.Remove(tmp)
		hub.StepFail(stepCoreJar)
		return "", err
	}
	f.Close()
	final := filepath.Join(dir, dl.Name)
	if err := os.Rename(tmp, final); err != nil {
		return "", err
	}
	hub.StepDone(stepCoreJar)
	hub.Broadcast("[MCS] 下载完成: " + dl.Name)
	return dl.Name, nil
}

// ===== Purpur（Paper 下游分支，配置项更多，性能同级）=====

const purpurAPI = "https://api.purpurmc.org/v2/purpur"

func purpurVersions() ([]string, error) {
	var data struct {
		Versions []string `json:"versions"`
	}
	if err := fetchJSON(purpurAPI, &data); err != nil {
		return nil, err
	}
	var out []string
	for _, v := range data.Versions {
		if isStableVersion(v) {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return versionLess(out[j], out[i]) })
	return out, nil
}

// downloadPurpur fetches the latest Purpur build jar for a version into dir.
func downloadPurpur(version, dir string, hub *ConsoleHub) (string, error) {
	hub.StepRun(stepCoreInfo)
	var info struct {
		Builds struct {
			Latest string `json:"latest"`
		} `json:"builds"`
	}
	if err := fetchJSON(fmt.Sprintf("%s/%s", purpurAPI, version), &info); err != nil {
		hub.StepFail(stepCoreInfo)
		return "", fmt.Errorf("获取 Purpur 构建信息失败: %w", err)
	}
	if info.Builds.Latest == "" {
		hub.StepFail(stepCoreInfo)
		return "", fmt.Errorf("Purpur 没有 %s 的可用构建", version)
	}
	hub.StepDone(stepCoreInfo)
	name := fmt.Sprintf("purpur-%s-%s.jar", version, info.Builds.Latest)
	hub.Broadcast(fmt.Sprintf("[MCS] 正在下载 Purpur %s build %s ...", version, info.Builds.Latest))
	u := fmt.Sprintf("%s/%s/%s/download", purpurAPI, version, info.Builds.Latest)
	if err := downloadWithProgress(u, filepath.Join(dir, name), hub, stepCoreJar); err != nil {
		hub.StepFail(stepCoreJar)
		return "", fmt.Errorf("下载 Purpur 失败: %w", err)
	}
	hub.StepDone(stepCoreJar)
	hub.Broadcast("[MCS] 下载完成: " + name)
	return name, nil
}

func writeServerFiles(dir string, port int, name string) error {
	if err := os.WriteFile(filepath.Join(dir, "eula.txt"), []byte("eula=true\n"), 0644); err != nil {
		return err
	}
	props := fmt.Sprintf("server-port=%d\nmotd=%s\nonline-mode=false\n", port, name)
	return os.WriteFile(filepath.Join(dir, "server.properties"), []byte(props), 0644)
}

// ===== Paper 核心一键更新 =====

var paperJarRe = regexp.MustCompile(`^paper-(.+)-(\d+)\.jar$`)

func latestPaperBuild(version string) (int, error) {
	var build struct {
		ID int `json:"id"`
	}
	if err := fetchJSON(fmt.Sprintf("%s/versions/%s/builds/latest", fillAPI, version), &build); err != nil {
		return 0, err
	}
	return build.ID, nil
}

// handleCoreCheck reports whether a newer Paper build exists for the instance.
func (m *Manager) handleCoreCheck(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	in, ok := m.insts[r.PathValue("id")]
	var typ, ver, jar string
	if ok {
		typ, ver, jar = in.Type, in.Version, in.JarFile
	}
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	if typ != "paper" {
		writeErr(w, 400, "目前只支持 Paper 核心更新")
		return
	}
	mt := paperJarRe.FindStringSubmatch(jar)
	if mt == nil {
		writeErr(w, 400, "无法识别当前核心版本")
		return
	}
	cur, _ := strconv.Atoi(mt[2])
	latest, err := latestPaperBuild(ver)
	if err != nil {
		writeErr(w, 502, "查询最新构建失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"version":      ver,
		"currentBuild": cur,
		"latestBuild":  latest,
		"hasUpdate":    latest > cur,
	})
}

// handleCoreUpdate downloads the latest Paper build and swaps the jar (server must be stopped).
func (m *Manager) handleCoreUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	if !ok {
		m.mu.Unlock()
		writeErr(w, 404, "实例不存在")
		return
	}
	rs := m.getRT(id)
	if rs.status != "stopped" && rs.status != "error" {
		m.mu.Unlock()
		writeErr(w, 400, "请先停止服务器再更新核心")
		return
	}
	if in.Type != "paper" {
		m.mu.Unlock()
		writeErr(w, 400, "目前只支持 Paper 核心更新")
		return
	}
	oldJar := in.JarFile
	ver := in.Version
	rs.status = "downloading"
	rs.console.ClearProgress()
	m.mu.Unlock()

	go func() {
		dir := m.instDir(id)
		newJar, err := downloadPaper(ver, dir, rs.console)
		m.mu.Lock()
		if err != nil {
			rs.status = "stopped"
			m.mu.Unlock()
			rs.console.Broadcast("[MCS] 核心更新失败: " + err.Error())
			m.addActivity("orange", fmt.Sprintf("<b>%s</b> 核心更新失败", in.Name))
			return
		}
		in.JarFile = newJar
		rs.status = "stopped"
		m.save()
		m.mu.Unlock()
		if oldJar != "" && oldJar != newJar {
			os.Remove(filepath.Join(dir, oldJar))
		}
		rs.console.Broadcast("[MCS] 核心已更新: " + oldJar + " → " + newJar)
		m.addActivity("green", fmt.Sprintf("<b>%s</b> 核心更新为 %s", in.Name, newJar))
		m.notify("服务端核心已更新", fmt.Sprintf("世界「%s」核心已从 %s 更新为 %s，下次启动生效。", in.Name, oldJar, newJar))
	}()
	writeJSON(w, 200, map[string]bool{"ok": true})
}
