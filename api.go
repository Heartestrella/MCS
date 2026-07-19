package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (m *Manager) handleList(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	out := make([]Instance, 0, len(m.insts))
	for _, in := range m.insts {
		out = append(out, m.snapshot(in))
	}
	m.mu.Unlock()
	writeJSON(w, 200, out)
}

func (m *Manager) handleGet(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	in, ok := m.insts[r.PathValue("id")]
	if !ok {
		m.mu.Unlock()
		writeErr(w, 404, "实例不存在")
		return
	}
	snap := m.snapshot(in)
	m.mu.Unlock()
	writeJSON(w, 200, snap)
}

type createReq struct {
	Name          string `json:"name"`
	Type          string `json:"type"` // paper(默认) / fabric / forge / neoforge
	Version       string `json:"version"`
	LoaderVersion string `json:"loaderVersion"` // 空 = 自动取最新稳定/推荐
	Port          int    `json:"port"`
	MemoryMB      int    `json:"memoryMB"`
	Dir           string `json:"dir"` // 自定义部署路径（可选）
}

// validateCustomDir checks a user-supplied deployment path: absolute, not inside
// the panel data dir, not colliding with another instance, and empty/creatable.
func (m *Manager) validateCustomDir(raw string) (string, error) {
	p := filepath.Clean(strings.TrimSpace(raw))
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("请填写绝对路径（例如 D:\\MCServers\\survival）")
	}
	if len(p) <= 3 { // "C:\" 之类的盘符根目录
		return "", fmt.Errorf("不能直接使用盘符根目录，请建一个子文件夹")
	}
	dataAbs, _ := filepath.Abs(m.dataDir)
	if strings.HasPrefix(strings.ToLower(p)+string(os.PathSeparator), strings.ToLower(dataAbs)+string(os.PathSeparator)) {
		return "", fmt.Errorf("该路径在面板数据目录内，无需自定义（留空即可）")
	}
	m.mu.Lock()
	for id := range m.insts {
		if strings.EqualFold(m.instDir(id), p) {
			m.mu.Unlock()
			return "", fmt.Errorf("该路径已被其他世界使用")
		}
	}
	m.mu.Unlock()
	if err := os.MkdirAll(p, 0755); err != nil {
		return "", fmt.Errorf("无法创建目录: %v", err)
	}
	if ents, err := os.ReadDir(p); err != nil {
		return "", fmt.Errorf("无法读取目录: %v", err)
	} else if len(ents) > 0 {
		return "", fmt.Errorf("目录不为空，请选择空文件夹或新路径")
	}
	probe := filepath.Join(p, ".mcs-write-test")
	if err := os.WriteFile(probe, nil, 0644); err != nil {
		return "", fmt.Errorf("目录不可写: %v", err)
	}
	os.Remove(probe)
	return p, nil
}

func (m *Manager) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if req.Name == "" || req.Version == "" {
		writeErr(w, 400, "名称和版本不能为空")
		return
	}
	if req.Port <= 0 {
		req.Port = 25565
	}
	if req.MemoryMB <= 0 {
		req.MemoryMB = 2048
	}

	typ := req.Type
	switch typ {
	case "", "paper":
		typ = "paper"
	case "purpur", "fabric", "forge", "neoforge":
	default:
		writeErr(w, 400, "不支持的服务端类型")
		return
	}

	in := &Instance{
		ID:        newID(),
		Name:      req.Name,
		Type:      typ,
		Version:   req.Version,
		Port:      req.Port,
		MemoryMB:  req.MemoryMB,
		CreatedAt: time.Now(),
	}
	if req.Dir != "" {
		p, err := m.validateCustomDir(req.Dir)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		in.Dir = p
		m.dirs.Store(in.ID, p)
	}
	dir := m.instDir(in.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
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

	typeName := map[string]string{"paper": "Paper", "purpur": "Purpur", "fabric": "Fabric", "forge": "Forge", "neoforge": "NeoForge"}[typ]
	m.addActivity("blue", fmt.Sprintf("正在创建新世界 <b>%s</b>（%s %s）", in.Name, typeName, in.Version))

	go func() {
		jar, err := m.installServerCore(typ, in.Version, req.LoaderVersion, dir, rs.console)
		m.mu.Lock()
		defer m.mu.Unlock()
		if err != nil {
			rs.status = "error"
			rs.errMsg = err.Error()
			rs.console.Broadcast("[MCS] 创建失败: " + err.Error())
			m.activity = append([]Activity{{Icon: "orange", Text: fmt.Sprintf("<b>%s</b> 创建失败", in.Name), Time: time.Now()}}, m.activity...)
			return
		}
		in.JarFile = jar
		if err := writeServerFiles(dir, in.Port, in.Name); err != nil {
			rs.status = "error"
			rs.errMsg = err.Error()
			return
		}
		rs.status = "stopped"
		m.save()
		rs.console.Broadcast("[MCS] 世界创建完成，可以启动了！")
		m.activity = append([]Activity{{Icon: "green", Text: fmt.Sprintf("<b>%s</b> 创建完成，随时可以开服", in.Name), Time: time.Now()}}, m.activity...)
		go m.notify("世界创建完成", fmt.Sprintf("世界「%s」（%s %s，端口 %d）已创建完成，随时可以开服。", in.Name, typeName, in.Version, in.Port))
	}()

	writeJSON(w, 201, m.snapshotLocked(in))
}

// installServerCore downloads/installs the server core for the given type.
// loaderVer 为空时自动取最新稳定/推荐版本。
func (m *Manager) installServerCore(typ, mc, loaderVer, dir string, hub *ConsoleHub) (string, error) {
	switch typ {
	case "fabric":
		if loaderVer == "" {
			var err error
			loaderVer, err = latestFabricLoader()
			if err != nil {
				return "", err
			}
		}
		return installFabricServer(dir, mc, loaderVer, hub)
	case "forge":
		if loaderVer == "" {
			var err error
			loaderVer, err = latestForgeFor(mc)
			if err != nil {
				return "", err
			}
		}
		return m.installForgeLike(dir, "forge", mc, loaderVer, hub)
	case "neoforge":
		if loaderVer == "" {
			var err error
			loaderVer, err = latestNeoForgeFor(mc)
			if err != nil {
				return "", err
			}
		}
		return m.installForgeLike(dir, "neoforge", mc, loaderVer, hub)
	case "purpur":
		return downloadPurpur(mc, dir, hub)
	default:
		return downloadPaper(mc, dir, hub)
	}
}

// latestFabricLoader returns the newest stable Fabric loader version.
func latestFabricLoader() (string, error) {
	var loaders []struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
	}
	if err := fetchJSON("https://meta.fabricmc.net/v2/versions/loader", &loaders); err != nil {
		return "", fmt.Errorf("获取 Fabric loader 版本失败: %w", err)
	}
	for _, l := range loaders {
		if l.Stable {
			return l.Version, nil
		}
	}
	if len(loaders) > 0 {
		return loaders[0].Version, nil
	}
	return "", fmt.Errorf("Fabric loader 列表为空")
}

// latestForgeFor returns the recommended (or latest) Forge build for a MC version.
func latestForgeFor(mc string) (string, error) {
	var promo struct {
		Promos map[string]string `json:"promos"`
	}
	if err := fetchJSON("https://files.minecraftforge.net/net/minecraftforge/forge/promotions_slim.json", &promo); err != nil {
		return "", fmt.Errorf("获取 Forge 版本失败: %w", err)
	}
	if v, ok := promo.Promos[mc+"-recommended"]; ok && v != "" {
		return v, nil
	}
	if v, ok := promo.Promos[mc+"-latest"]; ok && v != "" {
		return v, nil
	}
	return "", fmt.Errorf("Forge 不支持 Minecraft %s（该版本可能太新或太旧）", mc)
}

// latestNeoForgeFor maps mc 1.21.4 → newest 21.4.x from the NeoForge maven metadata.
func latestNeoForgeFor(mc string) (string, error) {
	req, _ := http.NewRequest("GET", "https://maven.neoforged.net/releases/net/neoforged/neoforge/maven-metadata.xml", nil)
	req.Header.Set("User-Agent", "mcs-panel/1.0")
	resp, err := dlClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("获取 NeoForge 版本失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	// mc "1.21.4" → 前缀 "21.4."; "1.21" → "21.0."
	rest := strings.TrimPrefix(mc, "1.")
	parts := strings.SplitN(rest, ".", 2)
	prefix := parts[0] + "."
	if len(parts) == 2 {
		prefix += parts[1] + "."
	} else {
		prefix += "0."
	}
	re := regexp.MustCompile(`<version>([^<]+)</version>`)
	var best string
	for _, mt := range re.FindAllStringSubmatch(string(raw), -1) {
		v := mt[1]
		if strings.HasPrefix(v, prefix) && !strings.Contains(v, "beta") {
			best = v // 列表按序排列，取最后一个匹配即最新
		}
	}
	if best == "" {
		// 放宽：接受 beta
		for _, mt := range re.FindAllStringSubmatch(string(raw), -1) {
			if strings.HasPrefix(mt[1], prefix) {
				best = mt[1]
			}
		}
	}
	if best == "" {
		return "", fmt.Errorf("NeoForge 不支持 Minecraft %s", mc)
	}
	return best, nil
}

// handleLoaderVersions lists installable loader versions for a type+mc version.
// GET /api/loaders?type=fabric&version=1.21.4 → {versions: [...], default: "x"}
func handleLoaderVersions(w http.ResponseWriter, r *http.Request) {
	typ := r.URL.Query().Get("type")
	mc := r.URL.Query().Get("version")
	switch typ {
	case "fabric":
		var loaders []struct {
			Version string `json:"version"`
			Stable  bool   `json:"stable"`
		}
		if err := fetchJSON("https://meta.fabricmc.net/v2/versions/loader", &loaders); err != nil {
			writeErr(w, 502, "获取 Fabric loader 列表失败: "+err.Error())
			return
		}
		out := []string{}
		def := ""
		for _, l := range loaders {
			out = append(out, l.Version)
			if def == "" && l.Stable {
				def = l.Version
			}
		}
		if len(out) > 30 {
			out = out[:30]
		}
		writeJSON(w, 200, map[string]any{"versions": out, "default": def})
	case "forge":
		if mc == "" {
			writeErr(w, 400, "缺少 version 参数")
			return
		}
		// 用 maven-metadata 列出该 MC 版本的全部 forge 构建
		req, _ := http.NewRequest("GET", "https://maven.minecraftforge.net/net/minecraftforge/forge/maven-metadata.xml", nil)
		req.Header.Set("User-Agent", "mcs-panel/1.0")
		resp, err := dlClient.Do(req)
		if err != nil {
			writeErr(w, 502, "获取 Forge 列表失败: "+err.Error())
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		re := regexp.MustCompile(`<version>` + regexp.QuoteMeta(mc) + `-([^<]+)</version>`)
		var out []string
		for _, mt := range re.FindAllStringSubmatch(string(raw), -1) {
			out = append(out, mt[1])
		}
		// 倒序（新在前），限 30
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
		if len(out) > 30 {
			out = out[:30]
		}
		def, _ := latestForgeFor(mc)
		writeJSON(w, 200, map[string]any{"versions": out, "default": def})
	case "neoforge":
		if mc == "" {
			writeErr(w, 400, "缺少 version 参数")
			return
		}
		req, _ := http.NewRequest("GET", "https://maven.neoforged.net/releases/net/neoforged/neoforge/maven-metadata.xml", nil)
		req.Header.Set("User-Agent", "mcs-panel/1.0")
		resp, err := dlClient.Do(req)
		if err != nil {
			writeErr(w, 502, "获取 NeoForge 列表失败: "+err.Error())
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		rest := strings.TrimPrefix(mc, "1.")
		parts := strings.SplitN(rest, ".", 2)
		prefix := parts[0] + "."
		if len(parts) == 2 {
			prefix += parts[1] + "."
		} else {
			prefix += "0."
		}
		re := regexp.MustCompile(`<version>([^<]+)</version>`)
		var out []string
		for _, mt := range re.FindAllStringSubmatch(string(raw), -1) {
			if strings.HasPrefix(mt[1], prefix) {
				out = append(out, mt[1])
			}
		}
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
		if len(out) > 30 {
			out = out[:30]
		}
		def := ""
		if len(out) > 0 {
			def = out[0]
			for _, v := range out { // 优先非 beta
				if !strings.Contains(v, "beta") {
					def = v
					break
				}
			}
		}
		writeJSON(w, 200, map[string]any{"versions": out, "default": def})
	default:
		writeJSON(w, 200, map[string]any{"versions": []string{}, "default": ""})
	}
}

// snapshotLocked is snapshot for callers NOT holding the lock.
func (m *Manager) snapshotLocked(in *Instance) Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshot(in)
}

// handleUpdate renames an instance / changes memory. Memory takes effect on restart.
func (m *Manager) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string  `json:"name"`
		MemoryMB    int     `json:"memoryMB"`
		AutoRestart *bool   `json:"autoRestart"`
		AutoSleep   *bool   `json:"autoSleep"`
		AutoStart   *bool   `json:"autoStart"`
		TalkInvite  *bool   `json:"talkInvite"`
		JavaPath    *string `json:"javaPath"` // ""=清空回自动匹配
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if body.JavaPath != nil && *body.JavaPath != "" {
		p := strings.TrimSpace(*body.JavaPath)
		if abs, err := filepath.Abs(p); err == nil {
			p = abs // 启动时 cmd.Dir 是实例目录，相对路径会失效
		}
		if fi, err := os.Stat(p); err != nil || fi.IsDir() {
			writeErr(w, 400, "Java 路径不存在或不是文件（应指向 java.exe）")
			return
		}
		if major := javaMajorOf(p); major == 0 {
			writeErr(w, 400, "该文件无法作为 Java 运行（java -version 执行失败）")
			return
		}
		*body.JavaPath = p
	}
	m.mu.Lock()
	in, ok := m.insts[r.PathValue("id")]
	if !ok {
		m.mu.Unlock()
		writeErr(w, 404, "实例不存在")
		return
	}
	oldName := in.Name
	if n := strings.TrimSpace(body.Name); n != "" && len([]rune(n)) <= 30 {
		in.Name = n
	}
	if body.MemoryMB >= 512 {
		in.MemoryMB = body.MemoryMB
	}
	if body.AutoRestart != nil {
		in.AutoRestart = *body.AutoRestart
	}
	if body.AutoStart != nil {
		in.AutoStart = *body.AutoStart
	}
	if body.TalkInvite != nil {
		in.TalkInvite = *body.TalkInvite
	}
	if body.JavaPath != nil {
		in.JavaPath = *body.JavaPath
	}
	sleepOff := false
	if body.AutoSleep != nil {
		in.AutoSleep = *body.AutoSleep
		sleepOff = !*body.AutoSleep && m.getRT(in.ID).status == "sleeping"
	}
	m.save()
	snap := m.snapshot(in)
	m.mu.Unlock()
	if sleepOff {
		m.stopWakeListener(in.ID)
		m.mu.Lock()
		m.getRT(in.ID).status = "stopped"
		m.mu.Unlock()
	}
	if oldName != snap.Name {
		m.addActivity("blue", fmt.Sprintf("世界 <b>%s</b> 改名为 <b>%s</b>", oldName, snap.Name))
	}
	writeJSON(w, 200, snap)
}

func (m *Manager) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	if !ok {
		m.mu.Unlock()
		writeErr(w, 404, "实例不存在")
		return
	}
	rs := m.getRT(id)
	if rs.status == "running" || rs.status == "starting" {
		m.mu.Unlock()
		writeErr(w, 409, "请先停止服务器再删除")
		return
	}
	delete(m.insts, id)
	delete(m.rt, id)
	m.save()
	name := in.Name
	inCopy := *in
	m.mu.Unlock()

	m.stopWakeListener(id)
	if _, err := m.moveToTrash(&inCopy); err != nil {
		// 移动失败（磁盘跨卷等）退回硬删除
		os.RemoveAll(m.instDir(id))
		m.addActivity("orange", fmt.Sprintf("世界 <b>%s</b> 已删除", name))
	} else {
		m.addActivity("orange", fmt.Sprintf("世界 <b>%s</b> 已移入回收站（7 天内可在备份页恢复）", name))
	}
	m.dirs.Delete(id)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (m *Manager) handleStart(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	in, ok := m.insts[r.PathValue("id")]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	if err := m.startInstance(in); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, m.snapshotLocked(in))
}

func (m *Manager) handleStop(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	in, ok := m.insts[r.PathValue("id")]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	if err := m.stopInstance(in); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, m.snapshotLocked(in))
}

// handleRestart stops the instance, waits for it to fully exit, then starts it again.
func (m *Manager) handleRestart(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	in, ok := m.insts[r.PathValue("id")]
	var st string
	if ok {
		st = m.getRT(in.ID).status
	}
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	if st != "running" && st != "starting" {
		// 没在运行就等于直接启动
		if err := m.startInstance(in); err != nil {
			writeErr(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"message": "服务器未在运行，已直接启动"})
		return
	}
	rs := m.getRTSafe(in.ID)
	rs.console.Broadcast("[MCS] 正在重启服务器…")
	if err := m.stopInstance(in); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	go func() {
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(2 * time.Second)
			m.mu.Lock()
			st := m.getRT(in.ID).status
			m.mu.Unlock()
			if st == "stopped" || st == "error" {
				break
			}
		}
		if err := m.startInstance(in); err != nil {
			rs.console.Broadcast("[MCS] 重启失败: " + err.Error())
			m.addActivity("orange", fmt.Sprintf("<b>%s</b> 重启失败: %s", in.Name, err.Error()))
		} else {
			m.addActivity("green", fmt.Sprintf("<b>%s</b> 已重启", in.Name))
		}
	}()
	writeJSON(w, 200, map[string]string{"message": "正在重启，停止后会自动再启动"})
}

// handleSleep manually puts a running instance into standby (stop + wake listener).
func (m *Manager) handleSleep(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	in, ok := m.insts[r.PathValue("id")]
	var st string
	if ok {
		st = m.getRT(in.ID).status
	}
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	if in.Type == "generic" {
		writeErr(w, 400, "通用服务器不支持待机唤醒")
		return
	}
	if st != "running" {
		writeErr(w, 409, "只有运行中的服务器才能进入待机")
		return
	}
	go m.goToSleep(in, true)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (m *Manager) handleCommand(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	in, ok := m.insts[r.PathValue("id")]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	var body struct {
		Command string `json:"command"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Command == "" {
		writeErr(w, 400, "command 不能为空")
		return
	}
	if err := m.sendCommand(in, body.Command); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (m *Manager) handleConsoleWS(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	in, ok := m.insts[r.PathValue("id")]
	var hub *ConsoleHub
	if ok {
		hub = m.getRT(in.ID).console
	}
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	hub.Serve(w, r, func(cmd string) error { return m.sendCommand(in, cmd) })
}

func (m *Manager) handleActivity(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	out := make([]Activity, len(m.activity))
	copy(out, m.activity)
	m.mu.Unlock()
	writeJSON(w, 200, out)
}

var versionCache struct {
	list []string
	at   time.Time
}

func handleVersions(w http.ResponseWriter, r *http.Request) {
	if time.Since(versionCache.at) < time.Hour && len(versionCache.list) > 0 {
		writeJSON(w, 200, versionCache.list)
		return
	}
	vs, err := paperVersions()
	if err != nil {
		writeErr(w, 502, "获取版本列表失败: "+err.Error())
		return
	}
	versionCache.list = vs
	versionCache.at = time.Now()
	writeJSON(w, 200, vs)
}

func (m *Manager) handleSystem(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"os":       runtime.GOOS,
		"arch":     runtime.GOARCH,
		"javaPath": findJava(),
	})
}
