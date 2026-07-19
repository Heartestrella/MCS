package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ===== AI 报错解析 + 一键处理 =====
// 兼容 OpenAI Chat Completions 协议（DeepSeek / 通义 / Kimi / OpenAI 等均可）。

type AIConfig struct {
	Enabled bool   `json:"enabled"`
	BaseURL string `json:"baseURL"` // 例如 https://api.deepseek.com/v1
	APIKey  string `json:"apiKey"`
	Model   string `json:"model"` // 例如 deepseek-chat
}

func (m *Manager) aiConfigPath() string { return filepath.Join(m.dataDir, "ai.json") }

func (m *Manager) loadAIConfig() AIConfig {
	cfg := AIConfig{BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat"}
	if b, err := os.ReadFile(m.aiConfigPath()); err == nil {
		json.Unmarshal(b, &cfg)
	}
	return cfg
}

func (m *Manager) handleAIGet(w http.ResponseWriter, r *http.Request) {
	cfg := m.loadAIConfig()
	if cfg.APIKey != "" {
		cfg.APIKey = "********"
	}
	writeJSON(w, 200, cfg)
}

func (m *Manager) handleAISet(w http.ResponseWriter, r *http.Request) {
	var cfg AIConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if cfg.APIKey == "" || cfg.APIKey == "********" {
		cfg.APIKey = m.loadAIConfig().APIKey
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(m.aiConfigPath(), b, 0600); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleAIModels lists available models from the OpenAI-compatible /models endpoint.
// 请求体可带 baseURL/apiKey 用「未保存的填写值」直接查询；留空则用已保存配置。
func (m *Manager) handleAIModels(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BaseURL string `json:"baseURL"`
		APIKey  string `json:"apiKey"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	saved := m.loadAIConfig()
	base := strings.TrimRight(strings.TrimSpace(body.BaseURL), "/")
	if base == "" {
		base = saved.BaseURL
	}
	key := strings.TrimSpace(body.APIKey)
	if key == "" || key == "********" {
		key = saved.APIKey
	}
	if base == "" || key == "" {
		writeErr(w, 400, "请先填写接口地址和 API Key")
		return
	}
	req, err := http.NewRequest("GET", base+"/models", nil)
	if err != nil {
		writeErr(w, 400, "接口地址无效")
		return
	}
	req.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		writeErr(w, 502, "请求失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		writeErr(w, 502, fmt.Sprintf("接口返回 HTTP %d（请检查地址和 Key）", resp.StatusCode))
		return
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Data) == 0 {
		writeErr(w, 502, "接口未返回模型列表")
		return
	}
	models := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		if d.ID != "" {
			models = append(models, d.ID)
		}
	}
	sort.Strings(models)
	writeJSON(w, 200, map[string]any{"models": models})
}

func (m *Manager) handleAITest(w http.ResponseWriter, r *http.Request) {
	cfg := m.loadAIConfig()
	out, err := aiChat(cfg, "你是测试助手。", "回复「连接成功」四个字。", 15*time.Second)
	if err != nil {
		writeErr(w, 502, "连接失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"reply": strings.TrimSpace(out)})
}

// aiChat calls an OpenAI-compatible chat completions endpoint.
func aiChat(cfg AIConfig, system, user string, timeout time.Duration) (string, error) {
	if cfg.APIKey == "" || cfg.BaseURL == "" {
		return "", fmt.Errorf("请先在设置页配置 AI API（地址 / 密钥 / 模型）")
	}
	body, _ := json.Marshal(map[string]any{
		"model": cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"temperature": 0.2,
		"max_tokens":  8192,
	})
	req, err := http.NewRequest("POST", cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("响应解析失败 (HTTP %d)", resp.StatusCode)
	}
	if out.Error != nil {
		return "", fmt.Errorf("%s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("AI 未返回内容 (HTTP %d)", resp.StatusCode)
	}
	return out.Choices[0].Message.Content, nil
}

// ===== 分析 =====

type AIAction struct {
	Type  string `json:"type"`  // set_prop / set_memory / disable_mod / command / manual
	Label string `json:"label"` // 按钮文字（给用户看）
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`
}

type AIAnalysis struct {
	Summary string     `json:"summary"`
	Causes  []string   `json:"causes"`
	Actions []AIAction `json:"actions"`
}

const aiSystemPrompt = `你是 Minecraft 开服助手，帮助完全不懂技术的玩家解决服务器问题。分析给出的服务器控制台日志，用通俗中文回答。
必须只输出一个 JSON 对象，不要 markdown 代码块，结构如下：
{
  "summary": "一两句话说清楚发生了什么（口语化，别用术语）",
  "causes": ["可能原因1", "可能原因2"],
  "actions": [
    {"type": "set_prop", "label": "把端口改成 25566", "key": "server-port", "value": "25566"},
    {"type": "set_memory", "label": "把内存调大到 4096MB", "value": "4096"},
    {"type": "disable_mod", "label": "禁用模组 xxx.jar", "value": "xxx.jar"},
    {"type": "command", "label": "在控制台执行 whitelist off", "value": "whitelist off"},
    {"type": "manual", "label": "需要手动操作的建议（无法自动执行）"}
  ]
}
规则：
- actions 只放真正能解决问题的操作，数量不限制：日志里点名了多少个问题模组，就给多少个 disable_mod，一个都不要漏；没有把握就用 manual 类型给建议
- 如果服务器因为模组/插件问题启动失败或崩溃（缺前置、版本冲突、加载报错、崩溃堆栈指向某个模组），必须在 actions 里给出 disable_mod 操作，逐个禁用日志中点名的问题模组文件
- 特别注意「仅客户端」模组：服务端日志里出现 NoClassDefFoundError / ClassNotFoundException 且类名带 net/minecraft/client 或 com/mojang/blaze3d，或提示 @OnlyIn(Dist.CLIENT)、environment: client、"is not on the server"，都说明该模组只能装在客户端。把日志里能对应上的这类模组全部列出来，每个单独一条 disable_mod，不要只挑前几个
- set_prop 只能改 server.properties 里的键
- disable_mod 的 value 必须是日志中出现的 mods/plugins 目录下的文件名
- 如果日志没有明显问题，summary 说明服务器状态正常，actions 为空数组`

func (m *Manager) handleAIAnalyze(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	cfg := m.loadAIConfig()
	if !cfg.Enabled {
		writeErr(w, 400, "AI 助手未启用，请到设置页配置")
		return
	}
	var opt struct {
		Force bool `json:"force"` // 用户点了「获取处理建议」：要求必须给出 actions
	}
	json.NewDecoder(r.Body).Decode(&opt)

	rs := m.getRTSafe(id)
	lines := rs.console.LaunchLog()
	if len(lines) == 0 {
		lines = rs.console.Recent(500)
	}
	if len(lines) == 0 {
		// 服务器没跑过：读最近一次日志文件
		if b, err := os.ReadFile(filepath.Join(m.instDir(id), "logs", "latest.log")); err == nil {
			lines = strings.Split(string(b), "\n")
		}
	}
	if len(lines) == 0 {
		writeErr(w, 400, "没有可分析的日志，先启动一次服务器")
		return
	}

	// 尽量把当次启动的日志全部交给 AI。超出预算时保头（mod 加载报错）保尾（崩溃堆栈），砍中间。
	const logBudget = 200_000
	logText := strings.Join(lines, "\n")
	if len(logText) > logBudget {
		head, tail := logBudget*2/5, logBudget*3/5
		logText = logText[:head] + "\n……（日志过长，中间已省略）……\n" + logText[len(logText)-tail:]
	}
	userMsg := fmt.Sprintf("服务器信息：类型 %s，Minecraft %s，端口 %d，内存 %dMB，当前状态 %s。\n\n最近控制台日志：\n%s",
		in.Type, in.Version, in.Port, in.MemoryMB, m.getRTSafe(id).status, logText)
	if files := m.listModFiles(id); len(files) > 0 {
		userMsg += "\n\n服务器已安装的模组/插件文件（disable_mod 的 value 必须从下面这些文件名里原样选取）：\n" + strings.Join(files, "\n")
	}
	if opt.Force {
		userMsg += "\n\n用户已明确要求获得处理建议：actions 数组不能为空。给出最可能解决问题的可执行操作；实在没有可自动执行的操作，也要用 manual 类型给出具体的手动操作步骤。"
	}

	reply, err := aiChat(cfg, aiSystemPrompt, userMsg, 180*time.Second)
	if err != nil {
		writeErr(w, 502, "AI 分析失败: "+err.Error())
		return
	}
	var res AIAnalysis
	raw := extractJSON(reply)
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		// 输出被 max_tokens 截断等导致 JSON 不完整：尝试修复后再解析
		if err2 := json.Unmarshal([]byte(repairJSON(raw)), &res); err2 != nil {
			// 模型没按格式输出时至少给出纯文本
			writeJSON(w, 200, AIAnalysis{Summary: strings.TrimSpace(reply)})
			return
		}
	}
	// 丢弃截断修复后可能残缺的建议（没有文字，或可执行操作缺 value）
	kept := res.Actions[:0]
	for _, a := range res.Actions {
		if a.Label == "" || (a.Type != "manual" && a.Value == "") || (a.Type == "set_prop" && a.Key == "") {
			continue
		}
		kept = append(kept, a)
	}
	res.Actions = kept
	m.addActivity("blue", fmt.Sprintf("<b>%s</b> 完成了一次 AI 日志分析", in.Name))
	writeJSON(w, 200, res)
}

var reJSONBlock = regexp.MustCompile(`(?s)\{.*\}`)

func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	if mt := reJSONBlock.FindString(s); mt != "" {
		return mt
	}
	// 没有闭合的 }：可能整体被截断，从第一个 { 起交给 repairJSON 处理
	if i := strings.IndexByte(s, '{'); i >= 0 {
		return s[i:]
	}
	return s
}

// repairJSON salvages a truncated JSON object: cut back to the end of the
// last complete value, then close whatever brackets remain open. 模型输出超长
// 被 max_tokens 截断时，至少保住已经写完整的那些建议。
func repairJSON(s string) string {
	var stack []byte
	inStr, esc := false, false
	lastIdx := -1
	var lastStack []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			stack = append(stack, c)
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			lastIdx = i
			lastStack = append(lastStack[:0], stack...)
		}
	}
	if lastIdx < 0 {
		return s
	}
	out := s[:lastIdx+1]
	for i := len(lastStack) - 1; i >= 0; i-- {
		if lastStack[i] == '{' {
			out += "}"
		} else {
			out += "]"
		}
	}
	return out
}

// listModFiles returns enabled mod/plugin filenames across modDirs (jar/zip only).
func (m *Manager) listModFiles(id string) []string {
	var out []string
	for _, sub := range modDirs {
		entries, err := os.ReadDir(filepath.Join(m.instDir(id), sub))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			low := strings.ToLower(e.Name())
			if strings.HasSuffix(low, ".jar") || strings.HasSuffix(low, ".zip") {
				out = append(out, e.Name())
			}
		}
	}
	sort.Strings(out)
	return out
}

// findModFile resolves an AI-given name to a real file path, tolerating
// mod-id vs filename mismatches (e.g. "create" → "create-1.20.1-0.5.1.jar").
func (m *Manager) findModFile(id, name string) string {
	base := strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(name), ".jar"), ".zip")
	if base == "" {
		return ""
	}
	norm := func(s string) string {
		return strings.NewReplacer("_", "", "-", "", " ", "").Replace(s)
	}
	nbase := norm(base)
	var prefix, contains string
	for _, sub := range modDirs {
		entries, err := os.ReadDir(filepath.Join(m.instDir(id), sub))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			low := strings.ToLower(e.Name())
			if !strings.HasSuffix(low, ".jar") && !strings.HasSuffix(low, ".zip") {
				continue
			}
			p := filepath.Join(m.instDir(id), sub, e.Name())
			if low == strings.ToLower(name) {
				return p // 精确命中
			}
			nlow := norm(low)
			if prefix == "" && strings.HasPrefix(nlow, nbase) {
				prefix = p
			} else if contains == "" && strings.Contains(nlow, nbase) {
				contains = p
			}
		}
	}
	if prefix != "" {
		return prefix
	}
	return contains
}

// ===== 一键执行 =====

func (m *Manager) handleAIApply(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	var act AIAction
	if err := json.NewDecoder(r.Body).Decode(&act); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}

	var msg string
	switch act.Type {
	case "set_prop":
		if act.Key == "" || strings.ContainsAny(act.Key, "=\n\r") || strings.ContainsAny(act.Value, "\n\r") {
			writeErr(w, 400, "无效的配置项")
			return
		}
		props, err := readProps(m.propsPath(id))
		if err != nil && !os.IsNotExist(err) {
			writeErr(w, 500, err.Error())
			return
		}
		if props == nil {
			props = map[string]string{}
		}
		props[act.Key] = act.Value
		var sb strings.Builder
		sb.WriteString("# Edited by MCS AI " + time.Now().Format("2006-01-02 15:04:05") + "\n")
		for k, v := range props {
			sb.WriteString(k + "=" + v + "\n")
		}
		if err := os.WriteFile(m.propsPath(id), []byte(sb.String()), 0644); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if act.Key == "server-port" {
			if p, _ := strconv.Atoi(act.Value); p > 0 {
				m.mu.Lock()
				in.Port = p
				m.save()
				m.mu.Unlock()
			}
		}
		msg = fmt.Sprintf("已修改配置 %s=%s，重启服务器生效", act.Key, act.Value)
	case "set_memory":
		mb, _ := strconv.Atoi(act.Value)
		if mb < 512 || mb > 65536 {
			writeErr(w, 400, "内存数值不合理")
			return
		}
		m.mu.Lock()
		in.MemoryMB = mb
		m.save()
		m.mu.Unlock()
		msg = fmt.Sprintf("已把最大内存调整为 %dMB，重启服务器生效", mb)
	case "disable_mod":
		name := filepath.Base(act.Value) // 防路径穿越
		if name == "" || name == "." || name == ".." {
			writeErr(w, 400, "无效的文件名")
			return
		}
		p := m.findModFile(id, name)
		if p == "" {
			writeErr(w, 404, "未找到该模组文件: "+name+"（可到「模组」页手动禁用）")
			return
		}
		if err := renameWithRetry(p, p+".disabled"); err != nil {
			writeErr(w, 500, "禁用失败: "+err.Error())
			return
		}
		msg = fmt.Sprintf("已禁用 %s（改名为 .disabled），重启服务器生效", filepath.Base(p))
	case "command":
		if act.Value == "" {
			writeErr(w, 400, "指令为空")
			return
		}
		if err := m.sendCommand(in, act.Value); err != nil {
			writeErr(w, 409, err.Error())
			return
		}
		msg = "已执行指令: " + act.Value
	default:
		writeErr(w, 400, "该建议需要手动操作")
		return
	}
	m.addActivity("green", fmt.Sprintf("<b>%s</b> AI 一键处理：%s", in.Name, msg))
	writeJSON(w, 200, map[string]string{"message": msg})
}
