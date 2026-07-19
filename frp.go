package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ===== 内网穿透（frp）=====
// 两种模式：
//   custom - 自定义 frps 服务器，面板自动下载官方 frpc 并生成 toml 配置
//   sakura - 樱花frp 等第三方定制客户端，用户填客户端路径和启动参数（如 -f token:隧道ID）

type FrpConfig struct {
	Mode         string `json:"mode"` // custom / sakura
	ServerAddr   string `json:"serverAddr"`
	ServerPort   int    `json:"serverPort"`
	Token        string `json:"token"`
	RemotePort   int    `json:"remotePort"`
	UDP          bool   `json:"udp"`      // 同时映射 UDP（基岩互通 19132 走 UDP）
	TalkPort     int    `json:"talkPort"` // 语音房 HTTPS 远程端口（custom 模式，0=不穿透）
	SakuraExe    string `json:"sakuraExe"`
	SakuraToken  string `json:"sakuraToken"`  // 樱花 访问密钥
	SakuraTunnel int    `json:"sakuraTunnel"` // 选中的隧道 ID
	SakuraName   string `json:"sakuraName"`   // 隧道名（展示用）
	SakuraArg    string `json:"sakuraArg"`    // 兼容旧配置：手填参数（有 token+tunnel 时忽略）
	AutoStart    bool   `json:"autoStart"`    // 服务器启动完成后自动开启穿透
	RawToml      bool   `json:"rawToml"`      // 用户改过配置源码，启动时不再重新生成
}

type frpRuntime struct {
	cmd    *exec.Cmd
	status string // stopped / running / error
	addr   string
	errMsg string
}

var (
	frpMu sync.Mutex
	frpRt = map[string]*frpRuntime{}
)

func frpGetRT(id string) *frpRuntime {
	rs, ok := frpRt[id]
	if !ok {
		rs = &frpRuntime{status: "stopped"}
		frpRt[id] = rs
	}
	return rs
}

func (m *Manager) frpDir() string               { return filepath.Join(m.dataDir, "frp") }
func (m *Manager) frpCfgPath(id string) string  { return filepath.Join(m.frpDir(), id+".json") }
func (m *Manager) frpTomlPath(id string) string { return filepath.Join(m.frpDir(), id+".toml") }

func (m *Manager) loadFrpConfig(id string) FrpConfig {
	cfg := FrpConfig{Mode: "custom", ServerPort: 7000}
	if b, err := os.ReadFile(m.frpCfgPath(id)); err == nil {
		json.Unmarshal(b, &cfg)
	}
	return cfg
}

func (m *Manager) saveFrpConfig(id string, cfg FrpConfig) error {
	os.MkdirAll(m.frpDir(), 0755)
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(m.frpCfgPath(id), b, 0600)
}

// ensureFrpc downloads the official frpc.exe (fatedier/frp latest release) once.
func (m *Manager) ensureFrpc(hub *ConsoleHub) (string, error) {
	exe := filepath.Join(m.frpDir(), "frpc.exe")
	if _, err := os.Stat(exe); err == nil {
		return exe, nil
	}
	os.MkdirAll(m.frpDir(), 0755)
	hub.Broadcast("[frp] 首次使用，正在下载 frpc 客户端（约 5MB，仅需一次）...")

	var rel struct {
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := fetchJSON("https://api.github.com/repos/fatedier/frp/releases/latest", &rel); err != nil {
		return "", fmt.Errorf("获取 frp 版本信息失败: %w", err)
	}
	var dlURL string
	for _, a := range rel.Assets {
		if strings.Contains(a.Name, "windows_amd64") && strings.HasSuffix(a.Name, ".zip") {
			dlURL = a.URL
			break
		}
	}
	if dlURL == "" {
		return "", fmt.Errorf("未找到 Windows 版 frpc 下载地址")
	}
	tmp := filepath.Join(m.frpDir(), "frp.zip")
	if err := downloadTo(dlURL, tmp); err != nil {
		return "", fmt.Errorf("下载 frpc 失败: %w", err)
	}
	defer os.Remove(tmp)
	exDir := filepath.Join(m.frpDir(), "_extract")
	defer os.RemoveAll(exDir)
	if err := unzip(tmp, exDir); err != nil {
		return "", fmt.Errorf("解压 frpc 失败: %w", err)
	}
	ms, _ := filepath.Glob(filepath.Join(exDir, "*", "frpc.exe"))
	if len(ms) == 0 {
		ms, _ = filepath.Glob(filepath.Join(exDir, "frpc.exe"))
	}
	if len(ms) == 0 {
		return "", fmt.Errorf("压缩包中未找到 frpc.exe")
	}
	if err := os.Rename(ms[0], exe); err != nil {
		b, rerr := os.ReadFile(ms[0])
		if rerr != nil {
			return "", rerr
		}
		if err := os.WriteFile(exe, b, 0755); err != nil {
			return "", err
		}
	}
	hub.Broadcast("[frp] frpc 客户端就绪")
	return exe, nil
}

func genFrpToml(cfg FrpConfig, in *Instance) string {
	var b strings.Builder
	fmt.Fprintf(&b, "serverAddr = %q\nserverPort = %d\n", cfg.ServerAddr, cfg.ServerPort)
	if cfg.Token != "" {
		fmt.Fprintf(&b, "auth.token = %q\n", cfg.Token)
	}
	fmt.Fprintf(&b, "\n[[proxies]]\nname = \"mcs-%s-tcp\"\ntype = \"tcp\"\nlocalIP = \"127.0.0.1\"\nlocalPort = %d\nremotePort = %d\n", in.ID, in.Port, cfg.RemotePort)
	if cfg.UDP {
		fmt.Fprintf(&b, "\n[[proxies]]\nname = \"mcs-%s-udp\"\ntype = \"udp\"\nlocalIP = \"127.0.0.1\"\nlocalPort = 19132\nremotePort = 19132\n", in.ID)
	}
	if cfg.TalkPort > 0 {
		// 语音房走面板 HTTPS 端口(TLS 直通,frp 只转 TCP 字节流)
		fmt.Fprintf(&b, "\n[[proxies]]\nname = \"mcs-%s-talk\"\ntype = \"tcp\"\nlocalIP = \"127.0.0.1\"\nlocalPort = %s\nremotePort = %d\n", in.ID, httpsPortStr, cfg.TalkPort)
	}
	return b.String()
}

var (
	reFrpOK   = regexp.MustCompile(`start .*proxy.* success|启动成功|创建隧道成功|映射启动成功`)
	reFrpAddr = regexp.MustCompile(`([A-Za-z0-9][\w.-]*\.[A-Za-z]{2,}:\d+|\d+\.\d+\.\d+\.\d+:\d+)`)
	reFrpErr  = regexp.MustCompile(`(?i)error|失败|token .*不正确|port .*already used`)
)

func (m *Manager) frpStart(in *Instance) error {
	frpMu.Lock()
	rs := frpGetRT(in.ID)
	if rs.status == "running" || rs.status == "preparing" {
		frpMu.Unlock()
		return fmt.Errorf("穿透已在运行")
	}
	frpMu.Unlock()

	cfg := m.loadFrpConfig(in.ID)
	hub := m.getRTSafe(in.ID).console

	if cfg.Mode == "sakura" {
		hasAuto := cfg.SakuraToken != "" && cfg.SakuraTunnel > 0 && cfg.SakuraExe != ""
		hasManual := cfg.SakuraExe != "" && cfg.SakuraArg != ""
		if !hasAuto && !hasManual {
			return fmt.Errorf("请先填写樱花访问密钥、选择隧道，并指定樱花 frpc 客户端路径")
		}
		if _, err := os.Stat(cfg.SakuraExe); err != nil {
			return fmt.Errorf("客户端不存在: %s", cfg.SakuraExe)
		}
	} else if cfg.ServerAddr == "" || cfg.RemotePort <= 0 {
		return fmt.Errorf("请先在穿透设置里填写 frps 服务器地址和远程端口")
	}

	frpMu.Lock()
	rs.status = "preparing"
	rs.errMsg = ""
	frpMu.Unlock()

	go func() {
		if err := m.frpLaunch(in, rs, cfg, hub); err != nil {
			frpMu.Lock()
			rs.status = "error"
			rs.errMsg = err.Error()
			frpMu.Unlock()
			hub.Broadcast("[frp] 启动失败: " + err.Error())
			m.addActivity("orange", fmt.Sprintf("<b>%s</b> 穿透启动失败", in.Name))
		}
	}()
	return nil
}

func (m *Manager) frpLaunch(in *Instance, rs *frpRuntime, cfg FrpConfig, hub *ConsoleHub) error {
	var cmd *exec.Cmd
	var expectAddr string
	if cfg.Mode == "sakura" {
		if cfg.SakuraToken != "" && cfg.SakuraTunnel > 0 {
			// 自动模式：樱花定制版 frpc 支持 -f 访问密钥:隧道ID
			if cfg.SakuraExe == "" {
				return fmt.Errorf("请填写樱花 frpc 客户端路径（natfrp.com 下载页的「frpc 单文件」）")
			}
			if _, err := os.Stat(cfg.SakuraExe); err != nil {
				return fmt.Errorf("客户端不存在: %s", cfg.SakuraExe)
			}
			cmd = exec.Command(cfg.SakuraExe, "-f", fmt.Sprintf("%s:%d", cfg.SakuraToken, cfg.SakuraTunnel))
			cmd.Dir = filepath.Dir(cfg.SakuraExe)
		} else {
			cmd = exec.Command(cfg.SakuraExe, strings.Fields(cfg.SakuraArg)...)
			cmd.Dir = filepath.Dir(cfg.SakuraExe)
		}
	} else {
		exe, err := m.ensureFrpc(hub)
		if err != nil {
			return err
		}
		toml := m.frpTomlPath(in.ID)
		if !cfg.RawToml {
			if err := os.WriteFile(toml, []byte(genFrpToml(cfg, in)), 0600); err != nil {
				return err
			}
		} else if _, err := os.Stat(toml); err != nil {
			os.WriteFile(toml, []byte(genFrpToml(cfg, in)), 0600)
		}
		cmd = exec.Command(exe, "-c", toml)
		cmd.Dir = m.frpDir()
		expectAddr = fmt.Sprintf("%s:%d", cfg.ServerAddr, cfg.RemotePort)
	}

	hideWindow(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 frpc 失败: %w", err)
	}
	assignToJob(cmd)

	frpMu.Lock()
	rs.cmd = cmd
	rs.status = "running"
	rs.addr = expectAddr
	rs.errMsg = ""
	frpMu.Unlock()
	hub.Broadcast("[frp] 穿透客户端已启动 ...")
	m.addActivity("green", fmt.Sprintf("<b>%s</b> 内网穿透已启动", in.Name))

	notified := false
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := cleanLine(toUTF8(sc.Bytes()))
			if line == "" {
				continue
			}
			hub.Broadcast("[frp] " + line)
			if reFrpOK.MatchString(line) {
				frpMu.Lock()
				if rs.addr == "" {
					if mt := reFrpAddr.FindString(line); mt != "" {
						rs.addr = mt
					}
				}
				addr := rs.addr
				frpMu.Unlock()
				if !notified {
					notified = true
					show := addr
					if show == "" {
						show = "（见控制台 [frp] 日志）"
					}
					hub.Broadcast("[frp] 隧道已连通！朋友用 " + show + " 即可直连")
					m.addActivity("green", fmt.Sprintf("<b>%s</b> 穿透隧道已连通 %s", in.Name, addr))
					m.notify("内网穿透已连通", fmt.Sprintf("世界「%s」的 frp 隧道已连通。\n公网连接地址：%s", in.Name, show))
				}
			} else if reFrpErr.MatchString(line) {
				frpMu.Lock()
				rs.errMsg = line
				frpMu.Unlock()
			}
		}
		cmd.Wait()
		frpMu.Lock()
		wasErr := rs.errMsg
		rs.cmd = nil
		if wasErr != "" && !notified {
			rs.status = "error"
		} else {
			rs.status = "stopped"
		}
		frpMu.Unlock()
		hub.Broadcast("[frp] 穿透客户端已退出")
		m.addActivity("blue", fmt.Sprintf("<b>%s</b> 内网穿透已停止", in.Name))
	}()
	return nil
}

func (m *Manager) frpStop(id string) error {
	frpMu.Lock()
	rs := frpGetRT(id)
	cmd := rs.cmd
	frpMu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return fmt.Errorf("穿透未在运行")
	}
	return cmd.Process.Kill()
}

// frpAutoStart is called when a server finishes booting (Done line).
func (m *Manager) frpAutoStart(in *Instance) {
	cfg := m.loadFrpConfig(in.ID)
	if !cfg.AutoStart {
		return
	}
	frpMu.Lock()
	running := frpGetRT(in.ID).status == "running"
	frpMu.Unlock()
	if running {
		return
	}
	if err := m.frpStart(in); err != nil {
		m.getRTSafe(in.ID).console.Broadcast("[frp] 自动启动穿透失败: " + err.Error())
	}
}

// ===== 樱花frp OpenAPI =====

// handleSakuraTunnels queries the user's tunnel list with their access key.
// GET /api/frp/sakura/tunnels?token=xxx （token 也可留空用已保存实例配置里的）
func (m *Manager) handleSakuraTunnels(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.URL.Query().Get("token"))
	if token == "" || token == "********" {
		// 尝试从指定实例的已保存配置里拿
		if id := r.URL.Query().Get("inst"); id != "" {
			token = m.loadFrpConfig(id).SakuraToken
		}
	}
	if token == "" {
		writeErr(w, 400, "请填写樱花访问密钥（官网 用户中心 → 访问密钥）")
		return
	}
	req, _ := http.NewRequest("GET", "https://api.natfrp.com/v4/tunnels", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "mcs-panel/1.0")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		writeErr(w, 502, "连接樱花 API 失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	// 401 等错误时樱花返回 {"code":401,"msg":"..."}
	var apiErr struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Code != 0 && apiErr.Msg != "" {
		writeErr(w, 502, "樱花 API: "+apiErr.Msg)
		return
	}
	// 隧道列表：兼容 数组 或 {tunnels:[...]} 两种形态
	var list []map[string]any
	if err := json.Unmarshal(body, &list); err != nil {
		var wrap struct {
			Tunnels []map[string]any `json:"tunnels"`
			Data    []map[string]any `json:"data"`
		}
		if json.Unmarshal(body, &wrap) == nil {
			if wrap.Tunnels != nil {
				list = wrap.Tunnels
			} else {
				list = wrap.Data
			}
		}
	}
	if list == nil {
		writeErr(w, 502, "樱花 API 返回了无法识别的数据，请检查密钥是否正确")
		return
	}
	writeJSON(w, 200, map[string]any{"tunnels": list})
}

// ===== HTTP handlers =====

func (m *Manager) handleFrpGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	_, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	cfg := m.loadFrpConfig(id)
	if cfg.Token != "" {
		cfg.Token = "********"
	}
	if cfg.SakuraToken != "" {
		cfg.SakuraToken = "********"
	}
	frpMu.Lock()
	rs := frpGetRT(id)
	resp := map[string]any{"config": cfg, "status": rs.status, "addr": rs.addr, "error": rs.errMsg}
	frpMu.Unlock()
	writeJSON(w, 200, resp)
}

func (m *Manager) handleFrpSet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	_, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	var cfg FrpConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	old := m.loadFrpConfig(id)
	if cfg.Token == "" || cfg.Token == "********" {
		cfg.Token = old.Token
	}
	if cfg.SakuraToken == "" || cfg.SakuraToken == "********" {
		cfg.SakuraToken = old.SakuraToken
	}
	cfg.RawToml = false // 表单保存 = 回到自动生成模式
	if err := m.saveFrpConfig(id, cfg); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	os.Remove(m.frpTomlPath(id)) // 下次启动按新表单重新生成
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (m *Manager) handleFrpStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	if err := m.frpStart(in); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (m *Manager) handleFrpStop(w http.ResponseWriter, r *http.Request) {
	if err := m.frpStop(r.PathValue("id")); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleFrpTomlGet returns the current (or freshly generated) frpc toml source.
func (m *Manager) handleFrpTomlGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	cfg := m.loadFrpConfig(id)
	b, err := os.ReadFile(m.frpTomlPath(id))
	if err != nil {
		b = []byte(genFrpToml(cfg, in))
	}
	writeJSON(w, 200, map[string]any{"content": string(b), "raw": cfg.RawToml})
}

func (m *Manager) handleFrpTomlSet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	_, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	os.MkdirAll(m.frpDir(), 0755)
	if err := os.WriteFile(m.frpTomlPath(id), []byte(req.Content), 0600); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	cfg := m.loadFrpConfig(id)
	cfg.RawToml = true
	m.saveFrpConfig(id, cfg)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
