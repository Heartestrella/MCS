package server

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ===== Modrinth 整合包（.mrpack）一键安装为服务端实例 =====

type mrFile struct {
	Path string `json:"path"`
	Env  struct {
		Server string `json:"server"`
	} `json:"env"`
	Downloads []string `json:"downloads"`
	FileSize  int64    `json:"fileSize"`

	// CurseForge 来源的内部字段（不参与 JSON 解析）
	Optional  bool `json:"-"`
	CFProject int  `json:"-"`
	CFFile    int  `json:"-"`
}

type mrIndex struct {
	FormatVersion int               `json:"formatVersion"`
	Name          string            `json:"name"`
	VersionID     string            `json:"versionId"`
	Files         []mrFile          `json:"files"`
	Dependencies  map[string]string `json:"dependencies"`
}

// ===== CurseForge 整合包（manifest.json，.zip/.mcpack）支持 =====

type cfManifest struct {
	Name      string `json:"name"`
	Minecraft struct {
		Version    string `json:"version"`
		ModLoaders []struct {
			ID      string `json:"id"` // e.g. "forge-47.2.20", "fabric-0.15.11"
			Primary bool   `json:"primary"`
		} `json:"modLoaders"`
	} `json:"minecraft"`
	Files []struct {
		ProjectID int  `json:"projectID"`
		FileID    int  `json:"fileID"`
		Required  bool `json:"required"`
	} `json:"files"`
	Overrides string `json:"overrides"`
}

// toIndex converts a CurseForge manifest into the internal mrIndex form.
// mod 文件此处只记录 projectID/fileID，真实文件名与下载地址在下载阶段解析
// （官网直链会 302 到带真实文件名的 CDN 地址）。
func (cf cfManifest) toIndex() mrIndex {
	idx := mrIndex{
		Name:         cf.Name,
		Dependencies: map[string]string{"minecraft": cf.Minecraft.Version},
	}
	loader := ""
	for _, ml := range cf.Minecraft.ModLoaders {
		if ml.Primary || loader == "" {
			loader = ml.ID
		}
	}
	if i := strings.Index(loader, "-"); i > 0 {
		kind, ver := loader[:i], loader[i+1:]
		switch kind {
		case "forge":
			idx.Dependencies["forge"] = ver
		case "neoforge":
			idx.Dependencies["neoforge"] = ver
		case "fabric":
			idx.Dependencies["fabric-loader"] = ver
		case "quilt":
			idx.Dependencies["quilt-loader"] = ver
		}
	}
	for _, f := range cf.Files {
		idx.Files = append(idx.Files, mrFile{
			Optional:  !f.Required,
			CFProject: f.ProjectID,
			CFFile:    f.FileID,
		})
	}
	return idx
}

// cfDownloadMod downloads one CurseForge mod into destDir/mods/.
// 首选 MCIM 镜像解析文件名 + CDN 镜像下载（国内可直连）；失败回退
// CurseForge 官网直链（302 到带真实文件名的 CDN 地址）。hub 用于测速统计。
// 返回实际保存的文件名；失败时错误里带上两条下载路径各自的原因，方便排查。
func cfDownloadMod(projectID, fileID int, destDir string, hub *ConsoleHub) (string, error) {
	direct := fmt.Sprintf("https://www.curseforge.com/api/v1/mods/%d/files/%d/download", projectID, fileID)
	mf, mirrorErr := cfResolveFile(projectID, fileID)
	if mirrorErr == nil {
		dest, err := safeJoin(destDir, mf.Path)
		if err != nil {
			return "", err
		}
		os.MkdirAll(filepath.Dir(dest), 0755)
		if mirrorErr = downloadToHub(mf.Downloads[0], dest, hub, ""); mirrorErr == nil {
			return filepath.Base(dest), nil
		}
	}
	// 兜底：官网直链（需要伪装浏览器请求，国内不一定通）
	name, directErr := downloadWithServerName(direct, filepath.Join(destDir, "mods"), hub)
	if directErr != nil {
		return "", fmt.Errorf("镜像下载失败（%v）；官网直链失败（%v）。可在浏览器打开 %s 手动下载后放入 mods 文件夹", mirrorErr, directErr, direct)
	}
	return name, nil
}

// browserUA: CurseForge 官网直链会 403 拒绝非浏览器 UA，必须伪装
const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"

// downloadWithServerName GETs a URL (following redirects) and saves it into dir,
// naming the file from Content-Disposition or the final redirected URL.
// hub 用于测速统计（可为 nil）。Returns the saved filename.
func downloadWithServerName(rawURL, dir string, hub *ConsoleHub) (string, error) {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", browserUA)
	// 尽量模拟浏览器导航请求，CurseForge 风控会检查这些头
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Referer", "https://www.curseforge.com/")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("sec-ch-ua", `"Not/A)Brand";v="8", "Chromium";v="126", "Google Chrome";v="126"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	resp, err := dlClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d（最终地址 %s）", resp.StatusCode, resp.Request.URL)
	}
	name := ""
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		name = params["filename"]
	}
	if name == "" && resp.Request != nil && resp.Request.URL != nil {
		if n, err := url.PathUnescape(path.Base(resp.Request.URL.Path)); err == nil {
			name = n
		}
	}
	name = filepath.Base(filepath.FromSlash(name)) // 去掉任何路径成分
	if name == "" || name == "." || name == string(filepath.Separator) || name == "download" {
		return "", fmt.Errorf("无法确定文件名（最终地址 %s）", resp.Request.URL)
	}
	os.MkdirAll(dir, 0755)
	dest := filepath.Join(dir, name)
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	body := withStallAbort(resp.Body)
	defer body.Close()
	written, err := copyWithProgress(f, body, resp.ContentLength, hub, "")
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("传输中断（最终地址 %s，已接收 %.1f MB）: %w", resp.Request.URL, float64(written)/1e6, err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	return name, nil
}

// cfResolveFile gets filename + download URL for a CurseForge projectID/fileID.
// 首选 MCIM 镜像的 CurseForge API（免 key、国内直连快），失败回退 cfwidget。
func cfResolveFile(projectID, fileID int) (mrFile, error) {
	var mcim struct {
		Data struct {
			FileName    string `json:"fileName"`
			DownloadURL string `json:"downloadUrl"`
		} `json:"data"`
	}
	mcimErr := fetchJSON(fmt.Sprintf("https://mod.mcimirror.top/curseforge/v1/mods/%d/files/%d", projectID, fileID), &mcim)
	if mcimErr == nil && mcim.Data.FileName != "" {
		dl := mcim.Data.DownloadURL
		if dl == "" {
			dl = fmt.Sprintf("https://mediafilez.forgecdn.net/files/%d/%d/%s",
				fileID/1000, fileID%1000, url.PathEscape(mcim.Data.FileName))
		}
		return mrFile{
			Path:      "mods/" + mcim.Data.FileName,
			Downloads: []string{dl},
		}, nil
	}
	var meta struct {
		Files []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"files"`
	}
	if err := fetchJSON(fmt.Sprintf("https://api.cfwidget.com/%d", projectID), &meta); err != nil {
		return mrFile{}, fmt.Errorf("MCIM 镜像（%v）与 cfwidget（%v）均查询失败", mcimErr, err)
	}
	name := ""
	for _, f := range meta.Files {
		if f.ID == fileID {
			name = f.Name
			break
		}
	}
	if name == "" {
		return mrFile{}, fmt.Errorf("项目 %d 中找不到文件 %d", projectID, fileID)
	}
	// CurseForge CDN: fileID 1234567 -> /files/1234/567/<name>
	u := fmt.Sprintf("https://mediafilez.forgecdn.net/files/%d/%d/%s",
		fileID/1000, fileID%1000, url.PathEscape(name))
	return mrFile{
		Path:      "mods/" + name,
		Downloads: []string{u},
	}, nil
}

// 允许的整合包文件下载源（Modrinth 规范 + CurseForge CDN）
var packDownloadHosts = map[string]bool{
	"cdn.modrinth.com":          true,
	"github.com":                true,
	"raw.githubusercontent.com": true,
	"gitlab.com":                true,
	"mediafilez.forgecdn.net":   true,
	"edge.forgecdn.net":         true,
}

func safeJoin(base, rel string) (string, error) {
	rel = filepath.FromSlash(rel)
	if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
		return "", fmt.Errorf("非法路径: %s", rel)
	}
	return filepath.Join(base, rel), nil
}

// mirrorHosts: MCIM 国内镜像（https://mcimirror.top）对应的加速域名。
// 命中这些域名的下载先走镜像、失败自动回退官方源。
var mirrorHosts = map[string]string{
	"cdn.modrinth.com":        "mod.mcimirror.top",
	"mediafilez.forgecdn.net": "mod.mcimirror.top",
	"edge.forgecdn.net":       "mod.mcimirror.top",
}

// candidateURLs returns download URLs in preference order: MCIM mirror first,
// original second. URLs on other hosts pass through unchanged.
func candidateURLs(rawURL string) []string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return []string{rawURL}
	}
	mh, ok := mirrorHosts[u.Host]
	if !ok {
		return []string{rawURL}
	}
	mu := *u
	mu.Host = mh
	return []string{mu.String(), rawURL}
}

// downloadTo fetches a URL into dest atomically，有镜像的源自动做
// 镜像→官方双源切换，全部失败才报错（错误里带两边各自的原因）。
func downloadTo(rawURL, dest string) error {
	return downloadToHub(rawURL, dest, nil, "")
}

// downloadToHub is downloadTo with optional console logging: hub 非空且 label
// 非空时，每个下载源的尝试、重定向后的最终地址、进度和失败明细都会打到控制台；
// label 为空则静默（只累计字节数用于测速），供并发下载 mod 时使用。
func downloadToHub(rawURL, dest string, hub *ConsoleHub, label string) error {
	urls := candidateURLs(rawURL)
	verbose := hub != nil && label != ""
	var errs []string
	for i, u := range urls {
		src := ""
		if len(urls) > 1 {
			src = "MCIM 镜像"
			if i > 0 {
				src = "官方源"
			}
		}
		if verbose {
			if src != "" {
				hub.Broadcast(fmt.Sprintf("[MCS] %s：尝试%s %s", label, src, u))
			} else {
				hub.Broadcast(fmt.Sprintf("[MCS] %s：%s", label, u))
			}
		}
		err := downloadToOne(u, dest, hub, label)
		if err == nil {
			return nil
		}
		if src != "" {
			errs = append(errs, fmt.Sprintf("%s：%v", src, err))
		} else {
			errs = append(errs, err.Error())
		}
		if verbose && i < len(urls)-1 {
			hub.Broadcast(fmt.Sprintf("[MCS] %s：%s失败（%v），切换下一个源 ...", label, src, err))
		}
	}
	return fmt.Errorf("%s", strings.Join(errs, "；"))
}

// downloadToOne fetches one URL into dest atomically (.part + rename).
// 错误信息带重定向后的最终地址、已接收字节数与耗时，方便定位是哪个 CDN 在断连。
func downloadToOne(rawURL, dest string, hub *ConsoleHub, label string) error {
	start := time.Now()
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "mcs-panel/1.0")
	resp, err := dlClient.Do(req)
	if err != nil {
		return fmt.Errorf("连接失败（%.1fs）: %w", time.Since(start).Seconds(), err)
	}
	defer resp.Body.Close()
	finalURL := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d（最终地址 %s）", resp.StatusCode, finalURL)
	}
	if hub != nil && label != "" && finalURL != rawURL {
		hub.Broadcast(fmt.Sprintf("[MCS] %s：重定向至 %s", label, finalURL))
	}
	body := withStallAbort(resp.Body)
	defer body.Close()
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	written, err := copyWithProgress(f, body, resp.ContentLength, hub, label)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		total := "未知大小"
		if resp.ContentLength > 0 {
			total = fmt.Sprintf("%.1f MB", float64(resp.ContentLength)/1e6)
		}
		return fmt.Errorf("传输中断（最终地址 %s，已接收 %.1f MB / %s，耗时 %.1fs）: %w",
			finalURL, float64(written)/1e6, total, time.Since(start).Seconds(), err)
	}
	return os.Rename(tmp, dest)
}

// handleModpackUpload creates a new instance from an uploaded local pack file
// (.mrpack / .mcpack / .zip — Modrinth or CurseForge format).
func (m *Manager) handleModpackUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<30)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeErr(w, 400, "上传失败: "+err.Error())
		return
	}
	name := r.FormValue("name")
	file, hdr, err := r.FormFile("file")
	if name == "" || err != nil {
		writeErr(w, 400, "参数不完整（需要 name 和 file）")
		return
	}
	defer file.Close()
	port := 25565
	if p, _ := strconv.Atoi(r.FormValue("port")); p > 0 {
		port = p
	}
	mem := 4096
	if v, _ := strconv.Atoi(r.FormValue("memoryMB")); v > 0 {
		mem = v
	}

	in := &Instance{
		ID:        newID(),
		Name:      name,
		Type:      "modpack",
		Port:      port,
		MemoryMB:  mem,
		CreatedAt: time.Now(),
	}
	dir := m.instDir(in.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	packPath := filepath.Join(dir, "pack.upload")
	out, err := os.Create(packPath)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		os.RemoveAll(dir)
		writeErr(w, 500, "保存上传文件失败: "+err.Error())
		return
	}
	out.Close()

	m.mu.Lock()
	m.insts[in.ID] = in
	rs := m.getRT(in.ID)
	rs.status = "downloading"
	rs.console.ClearProgress()
	m.save()
	m.mu.Unlock()

	m.addActivity("blue", fmt.Sprintf("正在导入本地整合包 <b>%s</b>（%s）", in.Name, esc0(hdr.Filename)))
	go func() {
		defer os.Remove(packPath)
		m.installPackFromFile(in, rs, packPath)
	}()
	writeJSON(w, 201, m.snapshotLocked(in))
}

func esc0(s string) string {
	return strings.NewReplacer("<", "&lt;", ">", "&gt;", "&", "&amp;").Replace(s)
}

// handleModpackInstall creates a new instance from a Modrinth .mrpack file.
func (m *Manager) handleModpackInstall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		URL      string `json:"url"`
		Filename string `json:"filename"`
		Port     int    `json:"port"`
		MemoryMB int    `json:"memoryMB"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" || body.URL == "" {
		writeErr(w, 400, "参数不完整")
		return
	}
	u, err := url.Parse(body.URL)
	if err != nil || u.Scheme != "https" || u.Host != "cdn.modrinth.com" {
		writeErr(w, 400, "仅允许从 Modrinth CDN 下载整合包")
		return
	}
	if body.Port <= 0 {
		body.Port = 25565
	}
	if body.MemoryMB <= 0 {
		body.MemoryMB = 4096
	}

	in := &Instance{
		ID:        newID(),
		Name:      body.Name,
		Type:      "modpack",
		Port:      body.Port,
		MemoryMB:  body.MemoryMB,
		CreatedAt: time.Now(),
	}
	if err := os.MkdirAll(m.instDir(in.ID), 0755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	m.mu.Lock()
	m.insts[in.ID] = in
	rs := m.getRT(in.ID)
	rs.status = "downloading"
	rs.console.ClearProgress()
	m.save()
	m.mu.Unlock()

	m.addActivity("blue", fmt.Sprintf("正在安装整合包 <b>%s</b>", in.Name))
	go m.installModpack(in, rs, body.URL)
	writeJSON(w, 201, m.snapshotLocked(in))
}

func (m *Manager) installModpack(in *Instance, rs *runtimeState, packURL string) {
	dir := m.instDir(in.ID)
	hub := rs.console

	hub.Broadcast("[MCS] 正在下载整合包文件（优先 MCIM 国内镜像，失败自动切官方源）...")
	packPath := filepath.Join(dir, "pack.mrpack")
	if err := downloadToHub(packURL, packPath, hub, "下载整合包"); err != nil {
		m.mu.Lock()
		rs.status = "error"
		rs.errMsg = "下载整合包失败: " + err.Error()
		m.mu.Unlock()
		hub.Broadcast("[MCS] 整合包安装失败: 下载整合包失败: " + err.Error())
		m.addActivity("orange", fmt.Sprintf("<b>%s</b> 整合包安装失败", in.Name))
		return
	}
	defer os.Remove(packPath)
	m.installPackFromFile(in, rs, packPath)
}

// mrVersion 是 Modrinth version 对象的子集，够用来解析依赖与取下载文件。
type mrVersion struct {
	ID           string   `json:"id"`
	ProjectID    string   `json:"project_id"`
	GameVersions []string `json:"game_versions"`
	Loaders      []string `json:"loaders"`
	Dependencies []struct {
		ProjectID      string `json:"project_id"`
		VersionID      string `json:"version_id"`
		DependencyType string `json:"dependency_type"`
	} `json:"dependencies"`
	Files []struct {
		URL      string `json:"url"`
		Filename string `json:"filename"`
		Primary  bool   `json:"primary"`
		Size     int64  `json:"size"`
	} `json:"files"`
}

// primaryFile 返回 version 的主文件（无主文件时取第一个）。
func (v mrVersion) primaryFile() (rawURL, filename string) {
	for _, f := range v.Files {
		if f.Primary || rawURL == "" {
			rawURL, filename = f.URL, f.Filename
		}
	}
	return
}

// modrinthIDsFromURL 从形如 /data/{project}/versions/{version}/{file} 的
// cdn.modrinth.com 直链里取出 (projectID, versionID)；非此形式返回空串。
func modrinthIDsFromURL(rawURL string) (string, string) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host != "cdn.modrinth.com" {
		return "", ""
	}
	p := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(p) >= 4 && p[0] == "data" && p[2] == "versions" {
		return p[1], p[3]
	}
	return "", ""
}

// resolveMissingDeps 沿整合包已装 mod 的依赖图，把"某个 mod 需要前置、但整合包
// 没带"的 required 前置补下载进 dir/mods，避免启动报 Missing mandatory dependencies。
// 尽力而为：单个前置解析/下载失败只记日志并跳过，绝不中断整包安装。
// installed 是本次实际下载的服务端文件（作为 BFS 起点），all 是清单里全部文件
// （用来判断某前置是否已被整合包带上，避免重复下载）。
func resolveMissingDeps(dir, mc, loader string, installed, all []mrFile, hub *ConsoleHub) {
	if loader == "" || mc == "" {
		return
	}
	have := map[string]bool{} // 已满足的 project id（整合包已带的）
	for _, f := range all {
		if len(f.Downloads) > 0 {
			if pid, _ := modrinthIDsFromURL(f.Downloads[0]); pid != "" {
				have[pid] = true
			}
		}
	}
	seen := map[string]bool{} // 已处理依赖的 version id，防环
	var queue []string
	for _, f := range installed {
		if len(f.Downloads) > 0 {
			if _, vid := modrinthIDsFromURL(f.Downloads[0]); vid != "" {
				queue = append(queue, vid)
			}
		}
	}

	gameFacet, _ := json.Marshal([]string{mc})
	loaderFacet, _ := json.Marshal([]string{loader})

	added := 0
	for len(queue) > 0 && added < 100 {
		vid := queue[0]
		queue = queue[1:]
		if seen[vid] {
			continue
		}
		seen[vid] = true

		var v mrVersion
		if err := modrinthGet("/version/"+url.PathEscape(vid), &v); err != nil {
			continue
		}
		for _, d := range v.Dependencies {
			if d.DependencyType != "required" {
				continue
			}
			var dep mrVersion
			switch {
			case d.VersionID != "":
				if err := modrinthGet("/version/"+url.PathEscape(d.VersionID), &dep); err != nil {
					continue
				}
			case d.ProjectID != "":
				if have[d.ProjectID] {
					continue
				}
				path := fmt.Sprintf("/project/%s/version?game_versions=%s&loaders=%s",
					url.PathEscape(d.ProjectID), url.QueryEscape(string(gameFacet)), url.QueryEscape(string(loaderFacet)))
				var vers []mrVersion
				if err := modrinthGet(path, &vers); err != nil || len(vers) == 0 {
					continue
				}
				dep = vers[0] // Modrinth 按发布时间倒序，取最新匹配版本
			default:
				continue
			}
			// 已满足则只追踪它的传递依赖，不重复下载
			if dep.ProjectID != "" && have[dep.ProjectID] {
				queue = append(queue, dep.ID)
				continue
			}
			du, dfn := dep.primaryFile()
			if du == "" || dfn == "" {
				continue
			}
			fu, err := url.Parse(du)
			if err != nil || fu.Scheme != "https" || !packDownloadHosts[fu.Host] {
				continue
			}
			dest, err := safeJoin(dir, "mods/"+dfn)
			if err != nil {
				continue
			}
			os.MkdirAll(filepath.Dir(dest), 0755)
			if dep.ProjectID != "" {
				have[dep.ProjectID] = true
			}
			queue = append(queue, dep.ID)
			if _, statErr := os.Stat(dest); statErr == nil {
				continue // 整合包已带同名文件
			}
			hub.Broadcast(fmt.Sprintf("[MCS] 检测到缺失前置：%s，正在补下载 ...", dfn))
			if err := downloadToHub(du, dest, hub, ""); err != nil {
				hub.Broadcast(fmt.Sprintf("[MCS] ✗ 前置 %s 下载失败: %v", dfn, err))
				continue
			}
			hub.Broadcast(fmt.Sprintf("[MCS] ✓ 已补齐前置 %s", dfn))
			added++
		}
	}
	if added > 0 {
		hub.Broadcast(fmt.Sprintf("[MCS] 共补齐 %d 个缺失前置模组", added))
	}
}

// installPackFromFile installs a local pack archive (Modrinth mrpack or
// CurseForge zip/mcpack) into the instance directory.
func (m *Manager) installPackFromFile(in *Instance, rs *runtimeState, packPath string) {
	dir := m.instDir(in.ID)
	hub := rs.console
	fail := func(msg string) {
		m.mu.Lock()
		rs.status = "error"
		rs.errMsg = msg
		m.mu.Unlock()
		hub.Broadcast("[MCS] 整合包安装失败: " + msg)
		m.addActivity("orange", fmt.Sprintf("<b>%s</b> 整合包安装失败", in.Name))
	}

	zr, err := zip.OpenReader(packPath)
	if err != nil {
		fail("整合包格式错误: " + err.Error())
		return
	}
	defer zr.Close()

	// 识别格式：Modrinth（modrinth.index.json）或 CurseForge（manifest.json）。
	// 兼容清单在 zip 根目录或一级子目录（例如「整合包名/manifest.json」）。
	var idx mrIndex
	kind, root := "", ""
	overrideDirs := []string{"overrides", "server-overrides"}
	for _, zf := range zr.File {
		base := path.Base(zf.Name)
		dir := path.Dir(zf.Name)
		if base != "modrinth.index.json" && base != "manifest.json" {
			continue
		}
		if dir != "." && strings.Contains(dir, "/") { // 最多一级子目录
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			fail(err.Error())
			return
		}
		if base == "modrinth.index.json" {
			err = json.NewDecoder(rc).Decode(&idx)
			kind = "modrinth"
		} else {
			var cf cfManifest
			if err = json.NewDecoder(rc).Decode(&cf); err == nil {
				idx = cf.toIndex()
				if cf.Overrides != "" {
					overrideDirs = []string{cf.Overrides, "server-overrides"}
				}
			}
			kind = "curseforge"
		}
		rc.Close()
		if err != nil {
			fail("解析整合包清单失败: " + err.Error())
			return
		}
		if dir != "." {
			root = dir + "/"
		}
		break
	}
	if kind == "" {
		fail("无法识别的整合包（缺少 modrinth.index.json 或 manifest.json）")
		return
	}

	// 1) 下载整合包声明的文件（跳过仅客户端/可选的）
	var files []mrFile
	skipped := 0
	for _, f := range idx.Files {
		if f.Env.Server == "unsupported" || f.Optional {
			skipped++
			continue
		}
		if f.CFProject > 0 {
			files = append(files, f)
			continue
		}
		if len(f.Downloads) == 0 {
			continue
		}
		fu, err := url.Parse(f.Downloads[0])
		if err != nil || fu.Scheme != "https" || !packDownloadHosts[fu.Host] {
			continue
		}
		files = append(files, f)
	}
	hub.Broadcast(fmt.Sprintf("[MCS] 整合包「%s」共 %d 个文件需下载（已跳过 %d 个仅客户端/可选文件）", idx.Name, len(files), skipped))

	// dlOne 下载单个整合包文件，返回用于日志的标签（成功时为落盘路径）。
	// 传 hub 但 label 为空：不刷单文件进度（并发时会互相覆盖），只累计测速字节
	dlOne := func(f mrFile) (string, error) {
		if f.CFProject > 0 {
			name, err := cfDownloadMod(f.CFProject, f.CFFile, dir, hub)
			if err != nil {
				return fmt.Sprintf("CurseForge %d/%d", f.CFProject, f.CFFile), err
			}
			return "mods/" + name, nil
		}
		dest, err := safeJoin(dir, f.Path)
		if err != nil {
			return f.Path, err
		}
		os.MkdirAll(filepath.Dir(dest), 0755)
		return f.Path, downloadToHub(f.Downloads[0], dest, hub, "")
	}

	var (
		wg     sync.WaitGroup
		sem    = make(chan struct{}, 16)
		prog   sync.Mutex
		done   int
		failed []mrFile
	)
	for _, f := range files {
		wg.Add(1)
		go func(f mrFile) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			label, err := dlOne(f)
			prog.Lock()
			done++
			if err != nil {
				failed = append(failed, f)
				hub.Broadcast(fmt.Sprintf("[MCS] ✗ (%d/%d) %s 下载失败: %v（稍后自动重试）", done, len(files), label, err))
			} else {
				hub.Broadcast(fmt.Sprintf("[MCS] ✓ (%d/%d) %s", done, len(files), label))
			}
			hub.SetProgress(fmt.Sprintf("下载整合包文件 %d/%d", done, len(files)), int64(done), int64(len(files)))
			prog.Unlock()
		}(f)
	}
	wg.Wait()

	// 失败的文件在其他任务完成后串行重试一轮（放慢节奏，避开 CDN 风控/限流）
	if len(failed) > 0 {
		hub.Broadcast(fmt.Sprintf("[MCS] 首轮有 %d 个文件失败，等待 5 秒后逐个重试 ...", len(failed)))
		time.Sleep(5 * time.Second)
		var errs []string
		for i, f := range failed {
			hub.SetProgress(fmt.Sprintf("重试失败文件 %d/%d", i+1, len(failed)), int64(i+1), int64(len(failed)))
			label, err := dlOne(f)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", label, err))
				hub.Broadcast(fmt.Sprintf("[MCS] ✗ 重试 (%d/%d) %s 仍然失败: %v", i+1, len(failed), label, err))
			} else {
				hub.Broadcast(fmt.Sprintf("[MCS] ✓ 重试 (%d/%d) %s", i+1, len(failed), label))
			}
			time.Sleep(2 * time.Second)
		}
		if len(errs) > 0 {
			hub.Broadcast(fmt.Sprintf("[MCS] 以下 %d 个文件重试后仍失败：", len(errs)))
			for _, e := range errs {
				hub.Broadcast("[MCS]   " + e)
			}
			fail(fmt.Sprintf("%d 个文件下载失败（明细见上方日志）", len(errs)))
			return
		}
		hub.Broadcast("[MCS] 重试全部成功！")
	}

	// 1.5) 补齐缺失前置：整合包里的 mod 声明了 required 前置但作者没打进包时，
	// 沿 Modrinth 依赖图自动下载，避免启动报 Missing mandatory dependencies。
	// 仅对 Modrinth 包生效（CurseForge 清单无依赖信息，files 里也没有 Modrinth 直链）。
	if kind == "modrinth" {
		depLoader := ""
		switch {
		case idx.Dependencies["fabric-loader"] != "":
			depLoader = "fabric"
		case idx.Dependencies["forge"] != "":
			depLoader = "forge"
		case idx.Dependencies["neoforge"] != "":
			depLoader = "neoforge"
		case idx.Dependencies["quilt-loader"] != "":
			depLoader = "quilt"
		}
		resolveMissingDeps(dir, idx.Dependencies["minecraft"], depLoader, files, idx.Files, hub)
	}

	// 2) 解压 overrides（server-overrides 优先级更高，后写覆盖）
	for _, od := range overrideDirs {
		prefix := root + strings.Trim(od, "/") + "/"
		for _, zf := range zr.File {
			if !strings.HasPrefix(zf.Name, prefix) || zf.FileInfo().IsDir() {
				continue
			}
			rel := strings.TrimPrefix(zf.Name, prefix)
			p, err := safeJoin(dir, rel)
			if err != nil {
				continue
			}
			os.MkdirAll(filepath.Dir(p), 0755)
			rc, err := zf.Open()
			if err != nil {
				continue
			}
			out, err := os.Create(p)
			if err != nil {
				rc.Close()
				continue
			}
			io.Copy(out, rc)
			out.Close()
			rc.Close()
		}
	}

	// 3) 安装服务端加载器
	mc := idx.Dependencies["minecraft"]
	if mc == "" {
		fail("整合包未声明 Minecraft 版本")
		return
	}
	var jarFile, loaderType string
	if lv, ok := idx.Dependencies["fabric-loader"]; ok {
		loaderType = "fabric"
		jarFile, err = installFabricServer(dir, mc, lv, hub)
	} else if fv, ok := idx.Dependencies["forge"]; ok {
		loaderType = "forge"
		jarFile, err = m.installForgeLike(dir, "forge", mc, fv, hub)
	} else if nv, ok := idx.Dependencies["neoforge"]; ok {
		loaderType = "neoforge"
		jarFile, err = m.installForgeLike(dir, "neoforge", mc, nv, hub)
	} else if _, ok := idx.Dependencies["quilt-loader"]; ok {
		err = fmt.Errorf("暂不支持 Quilt 服务端整合包")
	} else {
		err = fmt.Errorf("整合包未声明服务端加载器（fabric/forge/neoforge）")
	}
	if err != nil {
		fail(err.Error())
		return
	}

	if err := writeServerFiles(dir, in.Port, in.Name); err != nil {
		fail(err.Error())
		return
	}

	m.mu.Lock()
	in.Version = mc
	in.Type = loaderType
	in.JarFile = jarFile
	rs.status = "stopped"
	m.save()
	m.mu.Unlock()
	hub.Broadcast("[MCS] 整合包安装完成，可以启动了！")
	m.addActivity("green", fmt.Sprintf("整合包 <b>%s</b>（%s %s）安装完成", in.Name, loaderType, mc))
	m.notify("整合包安装完成", fmt.Sprintf("整合包「%s」（%s，Minecraft %s，端口 %d）已安装完成，随时可以开服。", in.Name, loaderType, mc, in.Port))
}

// installFabricServer downloads the single-jar Fabric server launcher.
func installFabricServer(dir, mc, loader string, hub *ConsoleHub) (string, error) {
	var installers []struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
	}
	if err := fetchJSON("https://meta.fabricmc.net/v2/versions/installer", &installers); err != nil {
		return "", fmt.Errorf("获取 Fabric 安装器版本失败: %v", err)
	}
	inst := ""
	for _, i := range installers {
		if i.Stable {
			inst = i.Version
			break
		}
	}
	if inst == "" && len(installers) > 0 {
		inst = installers[0].Version
	}
	if inst == "" {
		return "", fmt.Errorf("Fabric 安装器版本列表为空")
	}
	u := fmt.Sprintf("https://meta.fabricmc.net/v2/versions/loader/%s/%s/%s/server/jar",
		url.PathEscape(mc), url.PathEscape(loader), url.PathEscape(inst))
	hub.Broadcast(fmt.Sprintf("[MCS] 正在下载 Fabric 服务端（Minecraft %s, loader %s）...", mc, loader))
	jar := "fabric-server.jar"
	if err := downloadWithProgress(u, filepath.Join(dir, jar), hub, "下载 Fabric 服务端"); err != nil {
		return "", fmt.Errorf("下载 Fabric 服务端失败: %v", err)
	}
	return jar, nil
}

// javaProxyFlags converts the panel's HTTPS_PROXY/HTTP_PROXY env into JVM
// system properties, so installer subprocesses (which ignore env proxies)
// can download libraries through the same proxy.
func javaProxyFlags() []string {
	// 优先用面板配置的下载代理，未配置再回退环境变量
	var u *url.URL
	if p := dlProxy.Load(); p != nil {
		u = p
	} else {
		raw := os.Getenv("HTTPS_PROXY")
		if raw == "" {
			raw = os.Getenv("https_proxy")
		}
		if raw == "" {
			raw = os.Getenv("HTTP_PROXY")
		}
		if raw == "" {
			return nil
		}
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Hostname() == "" {
			return nil
		}
		u = parsed
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = "80"
	}
	// SOCKS 代理走 socksProxyHost；HTTP(S) 代理走 http(s).proxyHost
	if strings.HasPrefix(u.Scheme, "socks") {
		return []string{"-DsocksProxyHost=" + host, "-DsocksProxyPort=" + port}
	}
	return []string{
		"-Dhttp.proxyHost=" + host, "-Dhttp.proxyPort=" + port,
		"-Dhttps.proxyHost=" + host, "-Dhttps.proxyPort=" + port,
	}
}

// installForgeLike downloads the Forge/NeoForge installer and runs --installServer.
func (m *Manager) installForgeLike(dir, kind, mc, ver string, hub *ConsoleHub) (string, error) {
	javaPath, err := m.ensureJavaFor(mc, hub)
	if err != nil {
		return "", err
	}
	var instURLs []string
	var label string
	if kind == "forge" {
		full := mc + "-" + ver
		label = "Forge " + full
		rel := fmt.Sprintf("net/minecraftforge/forge/%s/forge-%s-installer.jar", full, full)
		// BMCLAPI 镜像优先（官方 maven 国内常 TLS 超时），官方回退
		instURLs = []string{
			"https://bmclapi2.bangbang93.com/maven/" + rel,
			"https://maven.minecraftforge.net/" + rel,
		}
	} else {
		label = "NeoForge " + ver
		rel := fmt.Sprintf("net/neoforged/neoforge/%s/neoforge-%s-installer.jar", ver, ver)
		instURLs = []string{
			"https://bmclapi2.bangbang93.com/maven/" + rel,
			"https://maven.neoforged.net/releases/" + rel,
		}
	}
	instJar := filepath.Join(dir, "installer.jar")
	hub.Broadcast("[MCS] 正在下载 " + label + " 安装器 ...")
	var dlErr error
	for i, u := range instURLs {
		src := "镜像源"
		if i > 0 {
			src = "官方源"
			hub.Broadcast(fmt.Sprintf("[MCS] 镜像源下载失败（%v），改用官方源重试 ...", dlErr))
		}
		if dlErr = downloadWithProgress(u, instJar, hub, fmt.Sprintf("下载 %s 安装器（%s）", label, src)); dlErr == nil {
			break
		}
	}
	if dlErr != nil {
		return "", fmt.Errorf("下载安装器失败（镜像与官方源均不可用）: %v", dlErr)
	}

	proxyFlags := javaProxyFlags()
	if len(proxyFlags) > 0 {
		hub.Broadcast("[MCS] 安装器将通过面板代理下载依赖库")
	}
	// 安装器偶发个别库下载超时，失败自动重试（已下载的库有校验缓存，重跑很快）
	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		if attempt > 1 {
			hub.Broadcast("[MCS] 部分依赖库下载失败，自动重试安装（已下载部分不会重复下载）...")
		}
		hub.Broadcast("[MCS] 正在安装 " + label + " 服务端（安装器会下载依赖库，可能需要几分钟）...")
		hub.SetProgress("安装 "+label+" 服务端（下载依赖库，需几分钟）...", 0, 0)
		args := append(append([]string{}, proxyFlags...), "-jar", "installer.jar", "--installServer")
		cmd := hideWindow(exec.Command(javaPath, args...))
		cmd.Dir = dir
		out, err := cmd.StdoutPipe()
		if err != nil {
			return "", err
		}
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			return "", err
		}
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		n := 0
		for sc.Scan() {
			n++
			if n%20 == 0 { // 安装器输出很多，只转发少量心跳行
				hub.Broadcast("[MCS] 安装器运行中: " + cleanLine(toUTF8(sc.Bytes())))
			}
		}
		if err := cmd.Wait(); err != nil {
			lastErr = fmt.Errorf("%s 安装器退出异常: %v", label, err)
			continue
		}
		lastErr = nil
		break
	}
	if lastErr != nil {
		return "", lastErr
	}
	os.Remove(instJar)
	os.Remove(instJar + ".log")

	// 新版：生成 run.bat / run.sh；旧版 Forge：universal jar 直接落在目录里
	if _, err := os.Stat(filepath.Join(dir, runScriptName())); err == nil {
		return runScriptName(), nil
	}
	if ms, _ := filepath.Glob(filepath.Join(dir, kind+"-*.jar")); len(ms) > 0 {
		for _, p := range ms {
			if !strings.Contains(filepath.Base(p), "installer") {
				return filepath.Base(p), nil
			}
		}
	}
	return "", fmt.Errorf("安装器完成但未找到启动文件（run.bat 或 %s-*.jar）", kind)
}
