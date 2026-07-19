package server

// talkbridge.go — MC 游戏内聊天 ⇆ 语音房文字互通。
// 游戏 → 语音房:pipeConsole 的 chatOnLine 命中后调 talkSrv.BroadcastGameChat。
// 语音房 → 游戏:talk chat 事件后调 mgr.talkChatToGame(tellraw 注入)。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// talkSrv is set once in main; nil until the talk server is constructed.
var talkSrv *TalkServer

// httpsPortStr mirrors the -https-port flag so links to the voice room can be
// built anywhere (status page, share links).
var httpsPortStr = "8446"

// talkChatToGame relays a voice-room text message into the running instance
// whose dedicated room matches. 用 tellraw 全员可见,紫色前缀区分来源。
func (m *Manager) talkChatToGame(room, name, text string) {
	m.mu.Lock()
	var target *Instance
	for id, in := range m.insts {
		if talkRoomOf(id) == room {
			rs := m.getRT(id)
			if rs.status == "running" {
				target = in
			}
			break
		}
	}
	m.mu.Unlock()
	if target == nil {
		return
	}
	if len(text) > 256 {
		text = text[:256]
	}
	// generic 服务器没有 tellraw;MC 系(paper/purpur/fabric/forge/neoforge/vanilla)都有
	if target.Type == "generic" {
		return
	}
	payload, _ := json.Marshal([]any{
		map[string]any{"text": "[语音房] ", "color": "light_purple"},
		map[string]any{"text": name + ": ", "color": "aqua"},
		map[string]any{"text": text, "color": "white"},
	})
	cmd := fmt.Sprintf("tellraw @a %s", payload)
	if err := m.sendCommandSilent(target, cmd); err == nil {
		// 服务器不回显 tellraw,自己写进面板聊天记录让两边都留痕
		chatOnLine(target.ID, fmt.Sprintf("]: [Server] [语音房] %s: %s", name, text))
	}
}

// sendCommandSilent writes a command to the instance stdin without echoing
// "> cmd" into the web console (avoids chat spam in the console view).
func (m *Manager) sendCommandSilent(in *Instance, command string) error {
	m.mu.Lock()
	rs := m.getRT(in.ID)
	stdin := rs.stdin
	ok := rs.status == "running"
	m.mu.Unlock()
	if !ok || stdin == nil {
		return fmt.Errorf("实例未在运行")
	}
	_, err := stdin.Write([]byte(command + "\r\n"))
	return err
}

// gameChatToTalk is called from chatOnLine for every parsed chat line.
func gameChatToTalk(instID, player, text string) {
	if talkSrv == nil || text == "" {
		return
	}
	// 语音房桥接过来的消息又被 chatOnLine 记录,别再回传形成回环
	if strings.HasPrefix(text, "[语音房] ") {
		return
	}
	talkSrv.BroadcastGameChat(instID, player, text)
}

// talkRoomActivity logs voice-room occupancy transitions (空→有人 / 有人→空)
// into the panel activity feed. Called from talk.go with counts after change.
func talkRoomActivity(room string, before, after int) {
	if talkSrv == nil || talkSrv.mgr == nil {
		return
	}
	m := talkSrv.mgr
	m.mu.Lock()
	var name string
	for id, in := range m.insts {
		if talkRoomOf(id) == room {
			name = in.Name
			break
		}
	}
	m.mu.Unlock()
	if name == "" {
		return // 非实例专属房间(手输房间号)不记录
	}
	if before == 0 && after > 0 {
		m.addActivity("green", fmt.Sprintf("<b>%s</b> 的语音房有人进入了", name))
	} else if before > 0 && after == 0 {
		m.addActivity("blue", fmt.Sprintf("<b>%s</b> 的语音房已无人", name))
	}
}

// talkURLFor returns the best voice-room URL for sharing: frp public address
// when the tunnel is running and TalkPort is set, otherwise LAN HTTPS.
func (m *Manager) talkURLFor(instID string) (url string, public bool) {
	room := talkRoomOf(instID)
	cfg := m.loadFrpConfig(instID)
	if cfg.Mode == "custom" && cfg.TalkPort > 0 && cfg.ServerAddr != "" {
		frpMu.Lock()
		running := frpGetRT(instID).status == "running"
		frpMu.Unlock()
		if running {
			return fmt.Sprintf("https://%s:%d/talk/#/room/%s", cfg.ServerAddr, cfg.TalkPort, room), true
		}
	}
	return fmt.Sprintf("https://%s:%s/talk/#/room/%s", lanIP(), httpsPortStr, room), false
}

// sendTalkInvite privately tellraws the voice-room URL to a player who just
// joined. 稍等 2 秒让客户端完全进入，clickEvent 可直接点开链接。
func (m *Manager) sendTalkInvite(in *Instance, player string) {
	if in.Type == "generic" {
		return
	}
	time.Sleep(2 * time.Second)
	url, _ := m.talkURLFor(in.ID)
	// 1.21.5 起文本组件改名: clickEvent→click_event(value→url), hoverEvent→hover_event
	link := map[string]any{"text": url, "color": "aqua", "underlined": true}
	if in.Version != "" && !versionLess(in.Version, "1.21.5") {
		link["click_event"] = map[string]any{"action": "open_url", "url": url}
	} else {
		link["clickEvent"] = map[string]any{"action": "open_url", "value": url}
	}
	payload, _ := json.Marshal([]any{
		map[string]any{"text": "[语音房] ", "color": "light_purple"},
		map[string]any{"text": "边玩边聊，点击加入: ", "color": "gray"},
		link,
	})
	m.sendCommandSilent(in, fmt.Sprintf("tellraw %s %s", player, payload))
}

// ===== 语音房密码管理(面板侧) =====

// handleTalkPwGet reports whether the instance's dedicated room has a password.
func (m *Manager) handleTalkPwGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !m.instExists(id) {
		writeErr(w, 404, "实例不存在")
		return
	}
	has := false
	if talkSrv != nil {
		room := talkRoomOf(id)
		talkSrv.mu.Lock()
		_, has = talkSrv.pw[room]
		talkSrv.mu.Unlock()
	}
	url, public := m.talkURLFor(id)
	writeJSON(w, 200, map[string]any{"hasPassword": has, "room": talkRoomOf(id), "url": url, "public": public})
}

// handleTalkPwSet sets or clears the room password. body: {"password": ""} 清除。
func (m *Manager) handleTalkPwSet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	if talkSrv == nil {
		writeErr(w, 500, "语音房服务未启动")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	room := talkRoomOf(id)
	talkSrv.mu.Lock()
	if body.Password == "" {
		delete(talkSrv.pw, room)
		talkSrv.savePwLocked()
	} else {
		talkSrv.setPassword(room, body.Password)
	}
	talkSrv.mu.Unlock()
	act := "已设置密码"
	if body.Password == "" {
		act = "已取消密码"
	}
	m.addActivity("blue", fmt.Sprintf("<b>%s</b> 的语音房%s", in.Name, act))
	writeJSON(w, 200, map[string]any{"hasPassword": body.Password != ""})
}
