package server

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ===== 远程面板模式：本页仅作 UI，所有 /api/* 反向代理到另一台面板 =====

const proxiedHeader = "X-MCS-Proxied"

type remoteState struct {
	mu    sync.Mutex
	url   *url.URL
	proxy *httputil.ReverseProxy
	asked bool // 已问询过（连接或选了"仅本机"都算），之后启动不再弹
}

var remote remoteState

type remoteConfig struct {
	URL   string `json:"url"`
	Asked bool   `json:"asked"`
}

func (m *Manager) remoteConfigPath() string { return filepath.Join(m.dataDir, "remote.json") }

func (m *Manager) loadRemote() {
	var cfg remoteConfig
	if b, err := os.ReadFile(m.remoteConfigPath()); err == nil {
		json.Unmarshal(b, &cfg)
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	remote.asked = cfg.Asked
	if cfg.URL != "" {
		if u, err := url.Parse(cfg.URL); err == nil {
			remote.url = u
			remote.proxy = newRemoteProxy(u)
		}
	}
}

func (m *Manager) saveRemote() {
	remote.mu.Lock()
	cfg := remoteConfig{Asked: remote.asked}
	if remote.url != nil {
		cfg.URL = remote.url.String()
	}
	remote.mu.Unlock()
	b, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(m.remoteConfigPath(), b, 0644)
}

// newRemoteProxy builds a reverse proxy to the target panel. Marks requests
// with proxiedHeader so a panel pointed at itself won't loop forever.
func newRemoteProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = target.Host
			r.Out.Header.Set(proxiedHeader, "1")
		},
		Transport: &http.Transport{
			// 面板自签 HTTPS 证书（tls.go），跳过校验
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeErr(w, 502, "远程面板连接失败: "+err.Error())
		},
	}
}

// probeRemote checks the target actually answers like a panel.
func probeRemote(u *url.URL) error {
	c := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	req, _ := http.NewRequest("GET", u.JoinPath("/api/auth/status").String(), nil)
	req.Header.Set(proxiedHeader, "1")
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("无法连接: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("远程返回 HTTP %d，不像是 MCS 面板地址", resp.StatusCode)
	}
	var st struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil || st.LoggedIn == nil {
		return fmt.Errorf("远程响应异常，不像是 MCS 面板地址")
	}
	return nil
}

func (m *Manager) handleRemoteStatus(w http.ResponseWriter, r *http.Request) {
	remote.mu.Lock()
	out := map[string]any{"connected": remote.proxy != nil, "asked": remote.asked, "url": ""}
	if remote.url != nil {
		out["url"] = remote.url.String()
	}
	remote.mu.Unlock()
	writeJSON(w, 200, out)
}

func (m *Manager) handleRemoteConnect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.URL) == "" {
		writeErr(w, 400, "请填写远程面板地址")
		return
	}
	raw := strings.TrimSpace(strings.TrimRight(body.URL, "/"))
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		writeErr(w, 400, "地址格式不正确，例如 http://192.168.1.10:8145")
		return
	}
	if err := probeRemote(u); err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	remote.mu.Lock()
	remote.url = u
	remote.proxy = newRemoteProxy(u)
	remote.asked = true
	remote.mu.Unlock()
	m.saveRemote()
	m.addActivity("blue", fmt.Sprintf("已连接到远程面板 <b>%s</b>，本面板仅作为界面", u.Host))
	writeJSON(w, 200, map[string]any{"ok": true, "url": u.String()})
}

func (m *Manager) handleRemoteSkip(w http.ResponseWriter, r *http.Request) {
	remote.mu.Lock()
	remote.asked = true
	remote.mu.Unlock()
	m.saveRemote()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (m *Manager) handleRemoteDisconnect(w http.ResponseWriter, r *http.Request) {
	remote.mu.Lock()
	was := ""
	if remote.url != nil {
		was = remote.url.Host
	}
	remote.url = nil
	remote.proxy = nil
	remote.asked = true
	remote.mu.Unlock()
	m.saveRemote()
	if was != "" {
		m.addActivity("blue", fmt.Sprintf("已断开远程面板 <b>%s</b>，回到本机模式", was))
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// remoteMiddleware forwards /api/* to the remote panel when one is configured.
// Static pages and /api/remote/* always stay local ("本页仅作为 UI").
func (m *Manager) remoteMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if !strings.HasPrefix(p, "/api/") ||
			strings.HasPrefix(p, "/api/remote") ||
			r.Header.Get(proxiedHeader) == "1" {
			next.ServeHTTP(w, r)
			return
		}
		remote.mu.Lock()
		proxy := remote.proxy
		remote.mu.Unlock()
		if proxy != nil {
			proxy.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
