package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ===== 面板访问密码（可选，开启后所有 API 和页面要求登录） =====

type AuthConfig struct {
	Enabled  bool   `json:"enabled"`
	Username string `json:"username"`
	Salt     string `json:"salt"`
	Hash     string `json:"hash"`  // sha256(salt + password)
	Setup    bool   `json:"setup"` // 首次初始化已完成
}

type session struct {
	expires time.Time
}

var (
	authMu       sync.Mutex
	authSessions = map[string]*session{}
)

const sessionTTL = 7 * 24 * time.Hour

func (m *Manager) authConfigPath() string { return filepath.Join(m.dataDir, "auth.json") }

func (m *Manager) loadAuth() AuthConfig {
	var cfg AuthConfig
	if b, err := os.ReadFile(m.authConfigPath()); err == nil {
		json.Unmarshal(b, &cfg)
	}
	if cfg.Hash != "" {
		cfg.Setup = true // 旧版本已设过密码的老用户不再弹初始化向导
	}
	return cfg
}

func hashPassword(salt, pw string) string {
	h := sha256.Sum256([]byte(salt + pw))
	return hex.EncodeToString(h[:])
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// handleAuthStatus tells the login page whether auth is enabled / session valid.
func (m *Manager) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	cfg := m.loadAuth()
	writeJSON(w, 200, map[string]any{
		"enabled":   cfg.Enabled,
		"loggedIn":  !cfg.Enabled || m.sessionValid(r),
		"needSetup": !cfg.Setup,
		"username":  cfg.Username,
	})
}

// handleAuthSetup completes the first-run admin wizard (only when not yet set up).
func (m *Manager) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	cfg := m.loadAuth()
	if cfg.Setup {
		writeErr(w, 409, "已初始化过，请在设置页修改账号")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Skip     bool   `json:"skip"` // 本机自用，跳过密码
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if body.Skip {
		cfg.Setup = true
		cfg.Enabled = false
	} else {
		body.Username = strings.TrimSpace(body.Username)
		if body.Username == "" || len(body.Username) > 32 {
			writeErr(w, 400, "请输入管理员账号名（32 字以内）")
			return
		}
		if len(body.Password) < 6 {
			writeErr(w, 400, "密码至少 6 位")
			return
		}
		cfg.Username = body.Username
		cfg.Salt = randHex(16)
		cfg.Hash = hashPassword(cfg.Salt, body.Password)
		cfg.Enabled = true
		cfg.Setup = true
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(m.authConfigPath(), b, 0600); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 初始化完成直接发一个会话，省得再登一次
	if cfg.Enabled {
		tok := randHex(32)
		authMu.Lock()
		authSessions[tok] = &session{expires: time.Now().Add(sessionTTL)}
		authMu.Unlock()
		http.SetCookie(w, &http.Cookie{
			Name: "mcs_session", Value: tok, Path: "/",
			HttpOnly: true, SameSite: http.SameSiteLaxMode,
			MaxAge: int(sessionTTL.Seconds()),
		})
	}
	m.addActivity("green", "管理员账号初始化完成")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleAuthSet enables/changes/disables the password. Requires current
// password when one is already set (enforced by middleware).
func (m *Manager) handleAuthSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled  bool   `json:"enabled"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	cfg := m.loadAuth()
	if u := strings.TrimSpace(body.Username); u != "" && len(u) <= 32 {
		cfg.Username = u
	}
	if body.Enabled {
		if body.Password == "" && cfg.Hash == "" {
			writeErr(w, 400, "请设置密码")
			return
		}
		if body.Password != "" {
			if len(body.Password) < 6 {
				writeErr(w, 400, "密码至少 6 位")
				return
			}
			cfg.Salt = randHex(16)
			cfg.Hash = hashPassword(cfg.Salt, body.Password)
		}
		cfg.Enabled = true
	} else {
		cfg.Enabled = false
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(m.authConfigPath(), b, 0600); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	m.addActivity("blue", fmt.Sprintf("面板访问密码已%s", map[bool]string{true: "开启", false: "关闭"}[cfg.Enabled]))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

var loginFails = struct {
	sync.Mutex
	m map[string][]time.Time
}{m: map[string][]time.Time{}}

// tooManyFails allows at most 5 failed attempts per IP per 10 minutes.
func tooManyFails(ip string) bool {
	loginFails.Lock()
	defer loginFails.Unlock()
	cutoff := time.Now().Add(-10 * time.Minute)
	keep := loginFails.m[ip][:0]
	for _, t := range loginFails.m[ip] {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	loginFails.m[ip] = keep
	return len(keep) >= 5
}

func recordFail(ip string) {
	loginFails.Lock()
	loginFails.m[ip] = append(loginFails.m[ip], time.Now())
	loginFails.Unlock()
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (m *Manager) handleLogin(w http.ResponseWriter, r *http.Request) {
	cfg := m.loadAuth()
	if !cfg.Enabled {
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	ip := clientIP(r)
	if tooManyFails(ip) {
		writeErr(w, 429, "尝试次数过多，请 10 分钟后再试")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	userOK := cfg.Username == "" || strings.EqualFold(strings.TrimSpace(body.Username), cfg.Username)
	got := hashPassword(cfg.Salt, body.Password)
	if !userOK || subtle.ConstantTimeCompare([]byte(got), []byte(cfg.Hash)) != 1 {
		recordFail(ip)
		writeErr(w, 401, "账号或密码错误")
		return
	}
	tok := randHex(32)
	authMu.Lock()
	authSessions[tok] = &session{expires: time.Now().Add(sessionTTL)}
	authMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: "mcs_session", Value: tok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: int(sessionTTL.Seconds()),
	})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (m *Manager) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("mcs_session"); err == nil {
		authMu.Lock()
		delete(authSessions, c.Value)
		authMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "mcs_session", Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (m *Manager) sessionValid(r *http.Request) bool {
	c, err := r.Cookie("mcs_session")
	if err != nil {
		return false
	}
	authMu.Lock()
	defer authMu.Unlock()
	s, ok := authSessions[c.Value]
	if !ok || time.Now().After(s.expires) {
		delete(authSessions, c.Value)
		return false
	}
	return true
}

// authMiddleware guards everything except the login endpoints and static page
// (the page itself checks /api/auth/status and shows the login overlay).
func (m *Manager) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if !strings.HasPrefix(p, "/api/") ||
			p == "/api/auth/status" || p == "/api/auth/login" || p == "/api/auth/setup" ||
			strings.HasPrefix(p, "/api/public/") {
			next.ServeHTTP(w, r)
			return
		}
		cfg := m.loadAuth()
		if cfg.Enabled && !m.sessionValid(r) {
			writeErr(w, 401, "需要登录")
			return
		}
		next.ServeHTTP(w, r)
	})
}
