package server

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ===== 联机诊断：一键排查「朋友连不上」 =====
// 逐项检查并给出通俗结论；防火墙问题可一键放行。

type netCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok / warn / fail / info
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"` // 可自动修复的动作标识
}

func runNetsh(args ...string) (string, error) {
	cmd := hideWindow(exec.Command("netsh", args...))
	out, err := cmd.CombinedOutput()
	return toUTF8(out), err
}

// firewallRuleExists checks for our MCS allow rule.
func firewallRuleExists() bool {
	out, err := runNetsh("advfirewall", "firewall", "show", "rule", "name=MCS Panel Game Server")
	return err == nil && !strings.Contains(out, "No rules match") && !strings.Contains(out, "没有与指定标准相匹配的规则")
}

// firewallActive reports whether any firewall profile is ON.
func firewallActive() bool {
	out, err := runNetsh("advfirewall", "show", "allprofiles", "state")
	if err != nil {
		return false
	}
	return strings.Contains(out, "ON") || strings.Contains(out, "打开")
}

func (m *Manager) handleNetDoctor(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}

	var checks []netCheck
	add := func(name, status, detail string, fix ...string) {
		c := netCheck{Name: name, Status: status, Detail: detail}
		if len(fix) > 0 {
			c.Fix = fix[0]
		}
		checks = append(checks, c)
	}

	// 1. 服务器状态
	rs := m.getRTSafe(id)
	m.mu.Lock()
	status := rs.status
	m.mu.Unlock()
	if status == "running" {
		add("服务器状态", "ok", "服务器正在运行")
	} else {
		add("服务器状态", "fail", "服务器没有在运行（状态: "+status+"），先启动它")
	}

	// 2. 端口监听
	listening := false
	if status == "running" {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", in.Port), 2*time.Second)
		if err == nil {
			conn.Close()
			listening = true
		}
	}
	if listening {
		add("端口监听", "ok", fmt.Sprintf("本机端口 %d 可以连通", in.Port))
	} else if status == "running" {
		add("端口监听", "fail", fmt.Sprintf("服务器在运行，但端口 %d 连不上——可能还在启动中，或端口配置不一致", in.Port))
	} else {
		add("端口监听", "info", "服务器未运行，跳过")
	}

	// 3. server-ip 错绑检查
	if props, err := readProps(m.propsPath(id)); err == nil {
		if ip := strings.TrimSpace(props["server-ip"]); ip != "" && ip != "0.0.0.0" {
			add("绑定地址", "fail", fmt.Sprintf("server.properties 里 server-ip=%s，这会导致别人连不上", ip), "clear_server_ip")
		} else {
			add("绑定地址", "ok", "server-ip 未绑定特定地址（正确）")
		}
		if props["online-mode"] == "true" {
			add("正版验证", "warn", "开启了正版验证：没买正版的朋友会被拒绝进入。可在服务器配置里关闭")
		} else {
			add("正版验证", "ok", "正版验证已关闭，离线玩家也能进")
		}
	}

	// 4. 防火墙
	if !firewallActive() {
		add("Windows 防火墙", "ok", "防火墙未开启，不会拦截")
	} else if firewallRuleExists() {
		add("Windows 防火墙", "ok", "已有 MCS 放行规则，不会拦截")
	} else {
		add("Windows 防火墙", "warn", "防火墙开着且没有放行规则，朋友的连接可能被拦截", "allow_firewall")
	}

	// 5. 局域网地址
	lan := lanIP()
	if lan != "" {
		add("局域网联机", "info", fmt.Sprintf("同一 WiFi/路由器下的朋友用 %s:%d 直连", lan, in.Port))
	} else {
		add("局域网联机", "warn", "无法确定本机局域网 IP，检查网络连接")
	}

	// 6. 公网方案状态
	frpMu.Lock()
	frpStatus := frpGetRT(id).status
	frpAddr := frpGetRT(id).addr
	frpMu.Unlock()
	if frpStatus == "running" {
		d := "frp 穿透运行中"
		if frpAddr != "" {
			d += "，异地朋友用 " + frpAddr + " 直连"
		}
		add("异地联机", "ok", d)
	} else {
		add("异地联机", "info", "不在同一网络的朋友需要：开启 UPnP 映射（路由器支持时）或 frp 内网穿透（配置页均可一键开启）")
	}

	writeJSON(w, 200, map[string]any{"checks": checks})
}

// handleNetFix applies an automatic fix suggested by the doctor.
func (m *Manager) handleNetFix(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	fix := r.URL.Query().Get("fix")
	switch fix {
	case "allow_firewall":
		// 放行 java + 面板数据目录下所有 java.exe，以及按端口放行
		if _, err := runNetsh("advfirewall", "firewall", "add", "rule",
			"name=MCS Panel Game Server", "dir=in", "action=allow",
			"protocol=TCP", fmt.Sprintf("localport=%d", in.Port)); err != nil {
			writeErr(w, 500, "添加防火墙规则失败（面板可能需要以管理员身份运行）")
			return
		}
		runNetsh("advfirewall", "firewall", "add", "rule",
			"name=MCS Panel Game Server", "dir=in", "action=allow",
			"protocol=UDP", "localport=19132")
		m.addActivity("green", fmt.Sprintf("<b>%s</b> 已添加防火墙放行规则", in.Name))
		writeJSON(w, 200, map[string]string{"message": "已放行端口，朋友可以重试连接了"})
	case "clear_server_ip":
		props, err := readProps(m.propsPath(id))
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		delete(props, "server-ip")
		var sb strings.Builder
		sb.WriteString("# Edited by MCS NetDoctor " + time.Now().Format("2006-01-02 15:04:05") + "\n")
		for k, v := range props {
			sb.WriteString(k + "=" + v + "\n")
		}
		if err := os.WriteFile(m.propsPath(id), []byte(sb.String()), 0644); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		m.addActivity("green", fmt.Sprintf("<b>%s</b> 已清除 server-ip 错误绑定", in.Name))
		writeJSON(w, 200, map[string]string{"message": "已清除 server-ip，重启服务器生效"})
	default:
		writeErr(w, 400, "未知修复动作")
	}
}
