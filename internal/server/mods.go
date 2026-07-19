package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Modrinth search proxy — server-side mods, plugins and modpacks with official page links.
const modrinthAPI = "https://api.modrinth.com/v2"

var modClient = &http.Client{Timeout: 30 * time.Second}

type modResult struct {
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Downloads   int64    `json:"downloads"`
	Follows     int64    `json:"follows"`
	IconURL     string   `json:"iconUrl"`
	PageURL     string   `json:"pageUrl"`
	ProjectType string   `json:"projectType"`
	Categories  []string `json:"categories"`
	Versions    []string `json:"versions"`
	ServerSide  string   `json:"serverSide"`
	ClientSide  string   `json:"clientSide"`
	Author      string   `json:"author"`
	Updated     string   `json:"updated"`
}

func modrinthGet(path string, v any) error {
	req, _ := http.NewRequest("GET", modrinthAPI+path, nil)
	req.Header.Set("User-Agent", "mcs-panel/1.0 (mcs local panel)")
	resp, err := modClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("Modrinth HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// handleModSearch proxies Modrinth search.
// Query params: q, type (mod|plugin|modpack|datapack), version, loader, category, sort, offset
func (m *Manager) handleModSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	projType := q.Get("type")
	if projType == "" {
		projType = "mod"
	}

	facets := [][]string{{"project_type:" + projType}}
	if projType == "mod" {
		// server-capable mods only (this is a server panel)
		facets = append(facets, []string{"server_side:required", "server_side:optional"})
	}
	if v := q.Get("version"); v != "" {
		facets = append(facets, []string{"versions:" + v})
	}
	if l := q.Get("loader"); l != "" {
		facets = append(facets, []string{"categories:" + l})
	}
	if c := q.Get("category"); c != "" {
		facets = append(facets, []string{"categories:" + c})
	}
	fb, _ := json.Marshal(facets)

	sort := q.Get("sort")
	if sort == "" {
		sort = "relevance"
	}
	offset := 0
	if o, err := strconv.Atoi(q.Get("offset")); err == nil && o > 0 {
		offset = o
	}

	params := url.Values{}
	params.Set("query", q.Get("q"))
	params.Set("facets", string(fb))
	params.Set("index", sort)
	params.Set("limit", "20")
	params.Set("offset", strconv.Itoa(offset))

	var raw struct {
		Hits []struct {
			Slug         string   `json:"slug"`
			Title        string   `json:"title"`
			Description  string   `json:"description"`
			Downloads    int64    `json:"downloads"`
			Follows      int64    `json:"follows"`
			IconURL      string   `json:"icon_url"`
			ProjectType  string   `json:"project_type"`
			Categories   []string `json:"categories"`
			Versions     []string `json:"versions"`
			ServerSide   string   `json:"server_side"`
			ClientSide   string   `json:"client_side"`
			Author       string   `json:"author"`
			DateModified string   `json:"date_modified"`
		} `json:"hits"`
		TotalHits int `json:"total_hits"`
	}
	if err := modrinthGet("/search?"+params.Encode(), &raw); err != nil {
		writeErr(w, 502, "模组检索失败: "+err.Error())
		return
	}

	out := make([]modResult, 0, len(raw.Hits))
	for _, h := range raw.Hits {
		kind := "mod"
		switch h.ProjectType {
		case "modpack":
			kind = "modpack"
		case "plugin":
			kind = "plugin"
		case "datapack":
			kind = "datapack"
		}
		out = append(out, modResult{
			Slug: h.Slug, Title: h.Title, Description: h.Description,
			Downloads: h.Downloads, Follows: h.Follows, IconURL: h.IconURL,
			PageURL:     "https://modrinth.com/" + h.ProjectType + "/" + h.Slug,
			ProjectType: kind, Categories: h.Categories, Versions: h.Versions,
			ServerSide: h.ServerSide, ClientSide: h.ClientSide,
			Author: h.Author, Updated: h.DateModified,
		})
	}
	writeJSON(w, 200, map[string]any{"hits": out, "total": raw.TotalHits, "offset": offset})
}

// handleModVersions lists downloadable files of a project (for install-to-instance).
func (m *Manager) handleModVersions(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	q := url.Values{}
	if gv := r.URL.Query().Get("version"); gv != "" {
		b, _ := json.Marshal([]string{gv})
		q.Set("game_versions", string(b))
	}
	if l := r.URL.Query().Get("loader"); l != "" {
		b, _ := json.Marshal([]string{l})
		q.Set("loaders", string(b))
	}
	var raw []struct {
		ID            string   `json:"id"`
		Name          string   `json:"name"`
		VersionNumber string   `json:"version_number"`
		GameVersions  []string `json:"game_versions"`
		Loaders       []string `json:"loaders"`
		DatePublished string   `json:"date_published"`
		Dependencies  []struct {
			ProjectID      string `json:"project_id"`
			DependencyType string `json:"dependency_type"`
		} `json:"dependencies"`
		Files []struct {
			URL      string `json:"url"`
			Filename string `json:"filename"`
			Primary  bool   `json:"primary"`
			Size     int64  `json:"size"`
		} `json:"files"`
	}
	path := "/project/" + url.PathEscape(slug) + "/version"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	if err := modrinthGet(path, &raw); err != nil {
		writeErr(w, 502, "获取版本失败: "+err.Error())
		return
	}
	type fileOut struct {
		ID       string   `json:"id"`
		Name     string   `json:"name"`
		Version  string   `json:"version"`
		Games    []string `json:"gameVersions"`
		Loaders  []string `json:"loaders"`
		URL      string   `json:"url"`
		Filename string   `json:"filename"`
		Size     int64    `json:"size"`
		Date     string   `json:"date"`
		Deps     []string `json:"deps,omitempty"` // required dependency project IDs
	}
	out := make([]fileOut, 0, len(raw))
	depIDs := map[string]bool{}
	for _, v := range raw {
		var u, fn string
		var sz int64
		for _, f := range v.Files {
			if f.Primary || u == "" {
				u, fn, sz = f.URL, f.Filename, f.Size
			}
		}
		var deps []string
		for _, d := range v.Dependencies {
			if d.DependencyType == "required" && d.ProjectID != "" {
				deps = append(deps, d.ProjectID)
				depIDs[d.ProjectID] = true
			}
		}
		out = append(out, fileOut{
			ID: v.ID, Name: v.Name, Version: v.VersionNumber,
			Games: v.GameVersions, Loaders: v.Loaders,
			URL: u, Filename: fn, Size: sz, Date: v.DatePublished,
			Deps: deps,
		})
		if len(out) >= 30 {
			break
		}
	}

	// resolve dependency project IDs → titles/slugs in one batch call
	depInfo := map[string]any{}
	if len(depIDs) > 0 {
		ids := make([]string, 0, len(depIDs))
		for id := range depIDs {
			ids = append(ids, id)
		}
		b, _ := json.Marshal(ids)
		var projs []struct {
			ID    string `json:"id"`
			Slug  string `json:"slug"`
			Title string `json:"title"`
		}
		if err := modrinthGet("/projects?ids="+url.QueryEscape(string(b)), &projs); err == nil {
			for _, p := range projs {
				depInfo[p.ID] = map[string]string{"slug": p.Slug, "title": p.Title}
			}
		}
	}
	writeJSON(w, 200, map[string]any{"versions": out, "deps": depInfo})
}

// ===== 模组更新检查 =====

// handleModUpdates checks each recorded install of an instance against the
// latest matching Modrinth version. Query: ?instId=...
func (m *Manager) handleModUpdates(w http.ResponseWriter, r *http.Request) {
	instID := r.URL.Query().Get("instId")
	m.mu.Lock()
	in, ok := m.insts[instID]
	var gameVer, instType string
	if ok {
		gameVer = in.Version
		instType = in.Type
	}
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}

	// loader facet by instance type
	loader := ""
	switch instType {
	case "paper":
		loader = "paper"
	case "purpur":
		loader = "purpur"
	case "fabric":
		loader = "fabric"
	case "forge":
		loader = "forge"
	case "neoforge":
		loader = "neoforge"
	}

	type updateInfo struct {
		Slug       string `json:"slug"`
		Title      string `json:"title"`
		Current    string `json:"current"` // installed filename
		LatestID   string `json:"latestId"`
		LatestName string `json:"latestName"`
		URL        string `json:"url"`
		Filename   string `json:"filename"`
		Kind       string `json:"kind"`
		HasUpdate  bool   `json:"hasUpdate"`
		Err        string `json:"err,omitempty"`
	}
	out := []updateInfo{}
	for _, rec := range m.readInstalls() {
		if rec.InstID != instID {
			continue
		}
		ui := updateInfo{Slug: rec.Slug, Title: rec.Title, Current: rec.Filename, Kind: rec.Kind}

		q := url.Values{}
		if gameVer != "" {
			b, _ := json.Marshal([]string{gameVer})
			q.Set("game_versions", string(b))
		}
		if loader != "" {
			b, _ := json.Marshal([]string{loader})
			q.Set("loaders", string(b))
		}
		var vers []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			VersionNumber string `json:"version_number"`
			DatePublished string `json:"date_published"`
			Files         []struct {
				URL      string `json:"url"`
				Filename string `json:"filename"`
				Primary  bool   `json:"primary"`
			} `json:"files"`
		}
		path := "/project/" + url.PathEscape(rec.Slug) + "/version"
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		if err := modrinthGet(path, &vers); err != nil {
			ui.Err = err.Error()
			out = append(out, ui)
			continue
		}
		if len(vers) > 0 {
			latest := vers[0] // Modrinth returns newest first
			ui.LatestID = latest.ID
			ui.LatestName = latest.Name
			if ui.LatestName == "" {
				ui.LatestName = latest.VersionNumber
			}
			for _, f := range latest.Files {
				if f.Primary || ui.URL == "" {
					ui.URL, ui.Filename = f.URL, f.Filename
				}
			}
			if latest.ID != rec.VersionID {
				// 已装版本在过滤列表里 → 直接比较位置；不在列表里（如按
				// 游戏版本过滤后被排除）→ 比较发布时间，避免把降级当更新
				inList := false
				for _, v := range vers {
					if v.ID == rec.VersionID {
						inList = true
						break
					}
				}
				if inList {
					ui.HasUpdate = true
				} else {
					var cur struct {
						DatePublished string `json:"date_published"`
					}
					if err := modrinthGet("/version/"+url.PathEscape(rec.VersionID), &cur); err == nil {
						ui.HasUpdate = latest.DatePublished > cur.DatePublished
					}
				}
			}
		}
		out = append(out, ui)
	}
	writeJSON(w, 200, out)
}

// handleModUpdate applies one update: download new file, remove the old one.
func (m *Manager) handleModUpdate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		InstID      string `json:"instId"`
		Slug        string `json:"slug"`
		Title       string `json:"title"`
		URL         string `json:"url"`
		Filename    string `json:"filename"`
		OldFilename string `json:"oldFilename"`
		Kind        string `json:"kind"`
		VersionID   string `json:"versionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.URL == "" || body.Filename == "" {
		writeErr(w, 400, "参数不完整")
		return
	}
	u, err := url.Parse(body.URL)
	if err != nil || u.Scheme != "https" || u.Host != "cdn.modrinth.com" {
		writeErr(w, 400, "仅允许从 Modrinth CDN 下载")
		return
	}
	for _, fn := range []string{body.Filename, body.OldFilename} {
		if fn != "" && (strings.ContainsAny(fn, `/\`) || strings.Contains(fn, "..")) {
			writeErr(w, 400, "非法文件名")
			return
		}
	}
	m.mu.Lock()
	in, ok := m.insts[body.InstID]
	var name string
	if ok {
		name = in.Name
	}
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}

	sub := "plugins"
	if body.Kind == "mod" {
		sub = "mods"
	} else if body.Kind == "datapack" {
		sub = filepath.Join("world", "datapacks")
	}
	destDir := filepath.Join(m.instDir(body.InstID), sub)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := downloadTo(body.URL, filepath.Join(destDir, body.Filename)); err != nil {
		writeErr(w, 502, "下载失败: "+err.Error())
		return
	}
	// remove old file (or its .disabled variant) if renamed
	if body.OldFilename != "" && body.OldFilename != body.Filename {
		os.Remove(filepath.Join(destDir, body.OldFilename))
		os.Remove(filepath.Join(destDir, body.OldFilename+".disabled"))
	}
	m.recordInstall(installRecord{
		InstID: body.InstID, Slug: body.Slug, Title: body.Title,
		VersionID: body.VersionID, Filename: body.Filename, Kind: body.Kind,
		At: time.Now(),
	})
	m.addActivity("green", fmt.Sprintf("<b>%s</b>：<b>%s</b> 已更新为 %s", name, body.Title, body.Filename))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ===== 安装记录（用于"已安装"标注） =====

type installRecord struct {
	InstID    string    `json:"instId"`
	Slug      string    `json:"slug"`
	Title     string    `json:"title"`
	VersionID string    `json:"versionId"`
	Filename  string    `json:"filename"`
	Kind      string    `json:"kind"`
	At        time.Time `json:"at"`
}

func (m *Manager) installsPath() string { return filepath.Join(m.dataDir, "installs.json") }

func (m *Manager) readInstalls() []installRecord {
	var out []installRecord
	if b, err := os.ReadFile(m.installsPath()); err == nil {
		json.Unmarshal(b, &out)
	}
	return out
}

// recordInstall appends/replaces the record for (instId, slug).
func (m *Manager) recordInstall(rec installRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := m.readInstalls()
	out := list[:0]
	for _, r := range list {
		if !(r.InstID == rec.InstID && r.Slug == rec.Slug) {
			out = append(out, r)
		}
	}
	out = append(out, rec)
	b, _ := json.MarshalIndent(out, "", "  ")
	os.WriteFile(m.installsPath(), b, 0644)
}

// handleInstallsList returns install records, pruned to files that still exist.
func (m *Manager) handleInstallsList(w http.ResponseWriter, r *http.Request) {
	list := m.readInstalls()
	out := make([]installRecord, 0, len(list))
	for _, rec := range list {
		sub := "plugins"
		switch rec.Kind {
		case "mod":
			sub = "mods"
		case "datapack":
			sub = filepath.Join("world", "datapacks")
		}
		p := filepath.Join(m.instDir(rec.InstID), sub, rec.Filename)
		if _, err := os.Stat(p); err == nil {
			out = append(out, rec)
		} else if _, err := os.Stat(p + ".disabled"); err == nil {
			out = append(out, rec)
		}
	}
	writeJSON(w, 200, out)
}

// handleModInstall downloads a mod/plugin file into an instance's plugins/ or mods/ dir.
func (m *Manager) handleModInstall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		InstID    string `json:"instId"`
		URL       string `json:"url"`
		Filename  string `json:"filename"`
		Kind      string `json:"kind"` // plugin / mod / datapack
		Slug      string `json:"slug"`
		Title     string `json:"title"`
		VersionID string `json:"versionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.URL == "" || body.Filename == "" {
		writeErr(w, 400, "参数不完整")
		return
	}
	u, err := url.Parse(body.URL)
	if err != nil || u.Scheme != "https" || u.Host != "cdn.modrinth.com" {
		writeErr(w, 400, "仅允许从 Modrinth CDN 下载")
		return
	}
	if strings.ContainsAny(body.Filename, `/\`) || strings.Contains(body.Filename, "..") {
		writeErr(w, 400, "非法文件名")
		return
	}

	m.mu.Lock()
	in, ok := m.insts[body.InstID]
	var name string
	if ok {
		name = in.Name
	}
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}

	sub := "plugins"
	if body.Kind == "mod" {
		sub = "mods"
	} else if body.Kind == "datapack" {
		sub = filepath.Join("world", "datapacks")
	}
	destDir := filepath.Join(m.instDir(body.InstID), sub)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	req, _ := http.NewRequest("GET", body.URL, nil)
	req.Header.Set("User-Agent", "mcs-panel/1.0")
	resp, err := modClient.Do(req)
	if err != nil {
		writeErr(w, 502, "下载失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		writeErr(w, 502, fmt.Sprintf("下载失败: HTTP %d", resp.StatusCode))
		return
	}
	dest := filepath.Join(destDir, body.Filename)
	tmp := dest + ".part"
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
	if err := os.Rename(tmp, dest); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	m.addActivity("green", fmt.Sprintf("<b>%s</b> 安装了 <b>%s</b>", name, body.Filename))
	if body.Slug != "" {
		m.recordInstall(installRecord{
			InstID: body.InstID, Slug: body.Slug, Title: body.Title,
			VersionID: body.VersionID, Filename: body.Filename, Kind: body.Kind,
			At: time.Now(),
		})
	}
	writeJSON(w, 200, map[string]any{"ok": true, "path": sub + "/" + body.Filename})
}
