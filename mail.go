package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type MailConfig struct {
	Enabled  bool   `json:"enabled"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	AuthCode string `json:"authCode"`
	To       string `json:"to"`
}

func (m *Manager) mailConfigPath() string { return filepath.Join(m.dataDir, "mail.json") }

func (m *Manager) loadMailConfig() MailConfig {
	cfg := MailConfig{Host: "smtp.qq.com", Port: 587}
	b, err := os.ReadFile(m.mailConfigPath())
	if err == nil {
		json.Unmarshal(b, &cfg)
	}
	return cfg
}

func (m *Manager) saveMailConfig(cfg MailConfig) error {
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(m.mailConfigPath(), b, 0600)
}

// sendMail sends a UTF-8 text mail using the stored config. Blocking; call in goroutine.
func sendMail(cfg MailConfig, subject, body string) error {
	if cfg.User == "" || cfg.AuthCode == "" {
		return fmt.Errorf("邮箱未配置")
	}
	to := cfg.To
	if to == "" {
		to = cfg.User
	}
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	auth := smtp.PlainAuth("", cfg.User, cfg.AuthCode, cfg.Host)

	b64subj := "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(subject)) + "?="
	msg := strings.Join([]string{
		"From: MCS Panel <" + cfg.User + ">",
		"To: " + to,
		"Subject: " + b64subj,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"Content-Transfer-Encoding: base64",
		"",
		base64.StdEncoding.EncodeToString([]byte(body)),
	}, "\r\n")

	return smtp.SendMail(addr, auth, cfg.User, []string{to}, []byte(msg))
}

// notify sends a progress mail if enabled; never blocks the caller.
func (m *Manager) notify(subject, body string) {
	cfg := m.loadMailConfig()
	if !cfg.Enabled {
		return
	}
	go func() {
		body = body + "\n\n—— MCS Panel · " + time.Now().Format("2006-01-02 15:04:05")
		if err := sendMail(cfg, "[MCS] "+subject, body); err != nil {
			m.addActivity("orange", "邮件通知发送失败: "+err.Error())
		}
	}()
}

func (m *Manager) handleMailGet(w http.ResponseWriter, r *http.Request) {
	cfg := m.loadMailConfig()
	if cfg.AuthCode != "" {
		cfg.AuthCode = "********" // never leak the code back to the page
	}
	writeJSON(w, 200, cfg)
}

func (m *Manager) handleMailSet(w http.ResponseWriter, r *http.Request) {
	var cfg MailConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if cfg.AuthCode == "" || cfg.AuthCode == "********" {
		cfg.AuthCode = m.loadMailConfig().AuthCode // keep existing
	}
	if cfg.Host == "" {
		cfg.Host = "smtp.qq.com"
	}
	if cfg.Port <= 0 {
		cfg.Port = 587
	}
	if err := m.saveMailConfig(cfg); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (m *Manager) handleMailTest(w http.ResponseWriter, r *http.Request) {
	cfg := m.loadMailConfig()
	if err := sendMail(cfg, "[MCS] 测试邮件", "这是一封来自 MCS 面板的测试邮件。收到即说明邮件通知配置成功。"); err != nil {
		writeErr(w, 502, "发送失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
