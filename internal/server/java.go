package server

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// javaMajorFor maps a Minecraft version to the Java major it needs.
//
//	<=1.16.x -> 8, 1.17.x–1.20.4 -> 17, 1.20.5–1.21.x -> 21, 26.1+（日历版本号）-> 25
func javaMajorFor(mc string) int {
	parts := strings.SplitN(strings.TrimSpace(mc), ".", 3)
	if len(parts) < 2 {
		return 25 // 无法识别（快照等）按最新处理
	}
	if major, err := strconv.Atoi(parts[0]); err != nil || major != 1 {
		return 25 // 26.x 起为日历版本号，需要 Java 25
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 25
	}
	patch := 0
	if len(parts) == 3 {
		p := parts[2]
		if i := strings.IndexAny(p, "-_ "); i > 0 {
			p = p[:i]
		}
		patch, _ = strconv.Atoi(p)
	}
	switch {
	case minor <= 16:
		return 8
	case minor <= 19 || (minor == 20 && patch <= 4):
		return 17
	default:
		return 21
	}
}

var javaVerRe = regexp.MustCompile(`version "(\d+)(?:\.(\d+))?`)

// reClassVer matches "class file version 65.0" → Java major = 65-44 = 21.
// 兼容 "has been compiled by a more recent version of the Java Runtime (class file version 65.0), this version ... up to 52.0"
var reClassVer = regexp.MustCompile(`class file version (\d+)`)

// reJavaTooNew matches Paper 老版本的自检提示：
// "Unsupported Java detected (65.0). Only up to Java 16 is supported."
var reJavaTooNew = regexp.MustCompile(`Only up to Java (\d+) is supported`)

// detectNeedJava parses a console line for Java-version-mismatch errors and
// returns the required Java major (0 = not a mismatch line).
// mcVersion 用于「Java 太新」场景推荐该 MC 版本的标准 Java。
func detectNeedJava(line, mcVersion string) int {
	if mt := reJavaTooNew.FindStringSubmatch(line); mt != nil {
		maxOK, _ := strconv.Atoi(mt[1])
		std := javaMajorFor(mcVersion)
		if std <= maxOK {
			return std // 推荐标准版本（如 1.16.5 → 8）
		}
		return maxOK
	}
	if !strings.Contains(line, "UnsupportedClassVersionError") && !strings.Contains(line, "class file version") {
		return 0
	}
	mt := reClassVer.FindStringSubmatch(line)
	if mt == nil {
		return 0
	}
	n, _ := strconv.Atoi(mt[1])
	if n < 52 || n > 90 { // Java 8 (52) ~ 未来版本上限
		return 0
	}
	return n - 44
}

// javaMajorOf runs `java -version` and parses the major (1.8 -> 8).
// 5 秒超时：防止把 GUI 程序当 java 校验时挂起。
func javaMajorOf(javaPath string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := hideWindow(exec.CommandContext(ctx, javaPath, "-version")).CombinedOutput()
	if err != nil {
		return 0
	}
	m := javaVerRe.FindSubmatch(out)
	if m == nil {
		return 0
	}
	major, _ := strconv.Atoi(string(m[1]))
	if major == 1 && len(m[2]) > 0 { // "1.8.0_xxx"
		major, _ = strconv.Atoi(string(m[2]))
	}
	return major
}

// findJavaMajor looks for an existing java of the given major:
// managed installs first (data/java/v<major>), then system installs.
func (m *Manager) findJavaMajor(major int) string {
	if p := findJavaIn(filepath.Join(m.dataDir, "java", fmt.Sprintf("v%d", major))); p != "" {
		return p
	}
	// 旧版面板把 JRE21 解压在 data/java/ 根下
	if major == 21 {
		for _, p := range findJavaAllIn(filepath.Join(m.dataDir, "java")) {
			if javaMajorOf(p) == 21 {
				return p
			}
		}
	}
	if p := findJava(); p != "" && javaMajorOf(p) == major {
		return p
	}
	return ""
}

// tunaJREURL scrapes the TUNA Adoptium mirror directory listing for the
// newest Temurin JRE archive of the given major for this OS/arch.
func tunaJREURL(major int) (string, error) {
	base := fmt.Sprintf("https://mirrors.tuna.tsinghua.edu.cn/Adoptium/%d/jre/%s/%s/", major, adoptiumArch(), adoptiumOS())
	req, _ := http.NewRequest("GET", base, nil)
	req.Header.Set("User-Agent", "mcs-panel/1.0")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := dlClient.Do(req.WithContext(ctx))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	re := regexp.MustCompile(fmt.Sprintf(`OpenJDK%dU-jre_%s_%s_hotspot_[0-9a-zA-Z._-]+%s`, major, adoptiumArch(), adoptiumOS(), regexp.QuoteMeta(jreArchiveExt())))
	names := re.FindAllString(string(body), -1)
	if len(names) == 0 {
		return "", fmt.Errorf("镜像目录中没有 Java %d 的 JRE 包", major)
	}
	sort.Strings(names) // 文件名带版本号，字典序最大即最新
	return base + names[len(names)-1], nil
}

// ensureJavaFor returns a java.exe of the major required by the given MC version,
// downloading a Temurin JRE into dataDir/java/v<major>/ when missing.
func (m *Manager) ensureJavaFor(mcVersion string, hub *ConsoleHub) (string, error) {
	return m.ensureJavaMajor(javaMajorFor(mcVersion), hub)
}

// ensureJavaMajor returns a java.exe of the exact major, downloading when missing.
func (m *Manager) ensureJavaMajor(major int, hub *ConsoleHub) (string, error) {
	if p := m.findJavaMajor(major); p != "" {
		return p, nil
	}

	javaDir := filepath.Join(m.dataDir, "java", fmt.Sprintf("v%d", major))
	if err := os.MkdirAll(javaDir, 0755); err != nil {
		return "", err
	}
	hub.Broadcast(fmt.Sprintf("[MCS] 正在自动下载 Java %d 运行环境（约 50MB，仅需一次）...", major))

	// 下载源：清华 TUNA 镜像优先（国内直连快）；失败回退 Adoptium 官方 API。
	// 官方 API 会 302 到 GitHub Releases，国内直连常慢到触发 30 分钟总超时。
	type dlSrc struct{ label, url string }
	var sources []dlSrc
	if u, err := tunaJREURL(major); err == nil {
		sources = append(sources, dlSrc{"清华 TUNA 镜像", u})
	} else {
		hub.Broadcast(fmt.Sprintf("[MCS] TUNA 镜像不可用（%v），将使用 Adoptium 官方源", err))
	}
	sources = append(sources, dlSrc{"Adoptium 官方",
		fmt.Sprintf("https://api.adoptium.net/v3/binary/latest/%d/ga/%s/%s/jre/hotspot/normal/eclipse", major, adoptiumOS(), adoptiumArch())})

	zipPath := filepath.Join(javaDir, "jre"+jreArchiveExt())
	var lastErr error
	for _, s := range sources {
		hub.Broadcast(fmt.Sprintf("[MCS] 正在从 %s 下载 Java %d ...", s.label, major))
		if lastErr = downloadWithProgress(s.url, zipPath, hub, fmt.Sprintf("下载 Java %d 运行环境", major)); lastErr == nil {
			break
		}
		hub.Broadcast(fmt.Sprintf("[MCS] %s 下载失败: %v", s.label, lastErr))
	}
	if lastErr != nil {
		return "", fmt.Errorf("下载 Java %d 失败（所有下载源均不可用）: %w", major, lastErr)
	}

	hub.Broadcast("[MCS] 正在解压 Java ...")
	hub.SetProgress(fmt.Sprintf("解压 Java %d ...", major), 0, 0)
	if err := extractArchive(zipPath, javaDir); err != nil {
		os.Remove(zipPath)
		return "", fmt.Errorf("解压 Java 失败: %w", err)
	}
	os.Remove(zipPath)

	if p := findJavaIn(javaDir); p != "" {
		hub.ClearProgress()
		hub.Broadcast(fmt.Sprintf("[MCS] Java %d 就绪: %s", major, p))
		m.notify(fmt.Sprintf("Java %d 自动安装完成", major),
			fmt.Sprintf("面板已自动下载并安装 Temurin JRE %d：\n%s", major, p))
		return p, nil
	}
	return "", fmt.Errorf("Java 解压后未找到 %s", javaBin())
}

// handleFixJava installs the Java major detected from the last startup failure,
// points the instance at it, and restarts the server.
func (m *Manager) handleFixJava(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	var need int
	var rs *runtimeState
	if ok {
		rs = m.getRT(id)
		need = rs.needJava
	}
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	if need == 0 {
		writeErr(w, 400, "当前没有待修复的 Java 版本问题")
		return
	}
	if rs.status == "running" || rs.status == "starting" || rs.status == "downloading" {
		writeErr(w, 409, "服务器正在运行或忙碌中")
		return
	}

	m.mu.Lock()
	rs.status = "downloading"
	rs.console.ClearProgress()
	m.mu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true, "installing": need})

	go func() {
		rs.console.Broadcast(fmt.Sprintf("[MCS] 正在安装 Java %d 并重启服务器 ...", need))
		p, err := m.ensureJavaMajor(need, rs.console)
		m.mu.Lock()
		if err != nil {
			rs.status = "error"
			rs.errMsg = "Java 安装失败: " + err.Error()
			m.mu.Unlock()
			rs.console.Broadcast("[MCS] " + rs.errMsg)
			return
		}
		in.JavaPath = p
		rs.needJava = 0
		rs.status = "stopped"
		m.save()
		m.mu.Unlock()
		rs.console.Broadcast(fmt.Sprintf("[MCS] 已切换到 Java %d：%s，正在启动 ...", need, p))
		m.addActivity("green", fmt.Sprintf("<b>%s</b> 已自动安装 Java %d 并重启", in.Name, need))
		if err := m.startInstance(in); err != nil {
			rs.console.Broadcast("[MCS] 重启失败: " + err.Error())
		}
	}()
}

// handleJavaInfo reports the instance's effective Java: custom path or the
// auto-matched managed/system install for its MC version.
func (m *Manager) handleJavaInfo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	var custom, ver, typ string
	if ok {
		custom, ver, typ = in.JavaPath, in.Version, in.Type
	}
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	out := map[string]any{"custom": custom}
	if typ == "generic" {
		out["mode"] = "generic"
		writeJSON(w, 200, out)
		return
	}
	need := javaMajorFor(ver)
	out["needMajor"] = need
	if custom != "" {
		out["mode"] = "custom"
		out["effective"] = custom
		out["effectiveMajor"] = javaMajorOf(custom)
	} else if p := m.findJavaMajor(need); p != "" {
		out["mode"] = "auto"
		out["effective"] = p
		out["effectiveMajor"] = need
	} else {
		out["mode"] = "auto"
		out["effective"] = "" // 首次启动时自动下载
	}
	writeJSON(w, 200, out)
}

func unzip(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	destAbs, _ := filepath.Abs(dest)
	for _, zf := range r.File {
		p := filepath.Join(dest, zf.Name)
		pAbs, _ := filepath.Abs(p)
		if !strings.HasPrefix(pAbs, destAbs+string(os.PathSeparator)) {
			return fmt.Errorf("zip 路径非法: %s", zf.Name)
		}
		if zf.FileInfo().IsDir() {
			os.MkdirAll(p, 0755)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			return err
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(p)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		out.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
