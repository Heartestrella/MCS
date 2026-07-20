package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// ProxyConfig 面板下载代理：开启后所有服务端核心/模组/整合包下载走此代理，
// 解决国内直连官方 CDN 太慢的问题。
type ProxyConfig struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"` // http://127.0.0.1:7890 / socks5://127.0.0.1:1080（可带 user:pass@）
}

// dlProxy 当前生效的下载代理地址；nil 表示不用固定代理、回退环境变量。
// 用 atomic 让设置页保存后即时生效，无需重启面板。
var dlProxy atomic.Pointer[url.URL]

func (m *Manager) proxyConfigPath() string { return filepath.Join(m.dataDir, "proxy.json") }

func (m *Manager) loadProxyConfig() ProxyConfig {
	var cfg ProxyConfig
	if b, err := os.ReadFile(m.proxyConfigPath()); err == nil {
		json.Unmarshal(b, &cfg)
	}
	return cfg
}

func (m *Manager) saveProxyConfig(cfg ProxyConfig) error {
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(m.proxyConfigPath(), b, 0600)
}

// applyProxyConfig 把配置装入 dlProxy，供 dlProxyFunc / javaProxyFlags 读取。
func (m *Manager) applyProxyConfig(cfg ProxyConfig) {
	if !cfg.Enabled || strings.TrimSpace(cfg.URL) == "" {
		dlProxy.Store(nil)
		return
	}
	u, err := parseProxyURL(cfg.URL)
	if err != nil {
		dlProxy.Store(nil)
		return
	}
	dlProxy.Store(u)
}

// dlProxyFunc 是 dlClient 的代理选择器：优先用面板配置的代理，
// 未配置时回退到 HTTP(S)_PROXY 等环境变量（保持旧行为）。
func dlProxyFunc(r *http.Request) (*url.URL, error) {
	if u := dlProxy.Load(); u != nil {
		return u, nil
	}
	return http.ProxyFromEnvironment(r)
}

// parseProxyURL 归一化用户填写的代理地址：缺协议时按 http 处理。
func parseProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("代理地址为空")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("代理地址格式错误: %w", err)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("不支持的代理协议 %q（支持 http/https/socks5）", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("代理地址缺少主机名")
	}
	return u, nil
}

func (m *Manager) handleProxyGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, m.loadProxyConfig())
}

func (m *Manager) handleProxySet(w http.ResponseWriter, r *http.Request) {
	var cfg ProxyConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	cfg.URL = strings.TrimSpace(cfg.URL)
	if cfg.Enabled {
		if _, err := parseProxyURL(cfg.URL); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	if err := m.saveProxyConfig(cfg); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	m.applyProxyConfig(cfg)
	state := "关闭"
	if cfg.Enabled {
		state = "开启（" + cfg.URL + "）"
	}
	m.addActivity("blue", "下载代理已"+state)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleProxyTest 用提交的代理（未提交则用已保存的）访问一次真实下载源，
// 验证代理能否连通并报告耗时。
func (m *Manager) handleProxyTest(w http.ResponseWriter, r *http.Request) {
	var body ProxyConfig
	json.NewDecoder(r.Body).Decode(&body)
	body.URL = strings.TrimSpace(body.URL)
	if body.URL == "" {
		body.URL = m.loadProxyConfig().URL
	}
	proxyURL, err := parseProxyURL(body.URL)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}
	const target = "https://fill.papermc.io/v3/projects/paper"
	start := time.Now()
	req, _ := http.NewRequest("GET", target, nil)
	req.Header.Set("User-Agent", "mcs-panel/1.0")
	resp, err := client.Do(req)
	if err != nil {
		writeErr(w, 502, "通过代理访问下载源失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		writeErr(w, 502, fmt.Sprintf("代理可连接，但下载源返回 HTTP %d", resp.StatusCode))
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":     true,
		"ms":     time.Since(start).Milliseconds(),
		"target": target,
	})
}
