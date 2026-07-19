package main

import (
	"net/http"
	"regexp"
	"sync"
	"time"
)

// ===== 游戏内聊天：面板查看 + 喊话 =====
// pipeConsole 解析聊天行存环形缓冲；玩家 tab 展示并可用 say/tellraw 喊话。

type chatMsg struct {
	T      time.Time `json:"t"`
	Player string    `json:"player"` // 空 = 系统/服主喊话
	Text   string    `json:"text"`
}

const chatKeep = 200

var (
	chatMu  sync.Mutex
	chatMap = map[string][]chatMsg{} // instID -> ring buffer
)

// 聊天行样式（1.19+ 可能带 [Not Secure] 前缀）:
//
//	"]: <Steve> hello" / "]: [Not Secure] <Steve> hello"
//	say 广播: "]: [Server] hi" / "]: [Not Secure] [Server] hi" / "[Rcon]"
var (
	reChat = regexp.MustCompile(`\]:\s+(?:\[Not Secure\]\s+)?<([A-Za-z0-9_]{1,16})>\s(.*)$`)
	reSay  = regexp.MustCompile(`\]:\s+(?:\[Not Secure\]\s+)?\[(?:Server|Rcon)\]\s(.*)$`)
)

// chatOnLine is called from pipeConsole for every console line.
func chatOnLine(instID, line string) {
	var player, text string
	if mt := reChat.FindStringSubmatch(line); mt != nil {
		player, text = mt[1], mt[2]
	} else if mt := reSay.FindStringSubmatch(line); mt != nil {
		player, text = "", mt[1]
	} else {
		return
	}
	chatMu.Lock()
	list := append(chatMap[instID], chatMsg{T: time.Now(), Player: player, Text: text})
	if len(list) > chatKeep {
		list = list[len(list)-chatKeep:]
	}
	chatMap[instID] = list
	chatMu.Unlock()
	gameChatToTalk(instID, player, text)
}

func (m *Manager) handleChatGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !m.instExists(id) {
		writeErr(w, 404, "实例不存在")
		return
	}
	chatMu.Lock()
	list := chatMap[id]
	out := make([]chatMsg, len(list))
	copy(out, list)
	chatMu.Unlock()
	writeJSON(w, 200, out)
}
