package main

// talk.go — Quick-Talk 语音房间中转(Go 重写版,替代原 Node/socket.io 后端)。
// 所有媒体(语音 PCM / 屏幕编码块 / 文字)经服务器广播转发,不落盘。
//
// 线路协议(自定义二进制帧,前端 wsio.js shim 对应实现):
//   frame  = u16BE headerLen | header JSON | attachment*  (拼接,长度在 header.bins)
//   header = {"ev": string, "args": [...], "bins": [len...]}
//   args 里的二进制参数用 {"$bin": idx} 占位,按序对应 attachment。

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var talkUpgrader = websocket.Upgrader{
	ReadBufferSize:  64 * 1024,
	WriteBufferSize: 64 * 1024,
	// 面板本身有 auth 中间件豁免 + 房间密码机制;跨端口页面也允许连
	CheckOrigin: func(r *http.Request) bool { return true },
}

const talkMaxFrame = 16 * 1024 * 1024 // 4K 关键帧上限,对齐原 maxHttpBufferSize

type talkPeer struct {
	id       string
	room     string
	name     string
	micOn    bool
	screenOn bool
	conn     *websocket.Conn
	send     chan []byte // 序列化好的帧;满了丢(音视频流宁丢不阻塞)
	closed   bool
}

type talkRoom struct {
	peers map[string]*talkPeer
}

type talkPwEntry struct {
	Salt      string `json:"salt"`
	Hash      string `json:"hash"`
	CreatedAt int64  `json:"createdAt"`
}

type TalkServer struct {
	mu      sync.Mutex
	rooms   map[string]*talkRoom
	pw      map[string]*talkPwEntry
	pwFile  string
	nextID  uint64
	saveTmr *time.Timer
	wt      *WTRelay // 可选 QUIC 通道,nil = 纯 WS
	mgr     *Manager // 语音房文字 → 游戏内 tellraw(nil = 不桥接)
}

// talkRoomOf maps an instance ID to its dedicated room name (id 前 8 位大写,
// 与前端 talkRoom() 一致)。
func talkRoomOf(instID string) string {
	r := strings.ToUpper(instID)
	if len(r) > 8 {
		r = r[:8]
	}
	return r
}

// BroadcastGameChat pushes an in-game chat line into the instance's voice room.
// player 为空表示服务器广播(say/面板喊话)。
func (t *TalkServer) BroadcastGameChat(instID, player, text string) {
	room := talkRoomOf(instID)
	name := "[游戏] " + player
	if player == "" {
		name = "[游戏] 服务器"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if r := t.rooms[room]; r == nil || len(r.peers) == 0 {
		return
	}
	t.broadcastLocked(room, "", buildTalkFrame("chat", []json.RawMessage{jraw(map[string]any{
		"from": "game:" + player, "name": name, "text": text, "ts": time.Now().UnixMilli(),
	})}, nil))
}

func NewTalkServer(dataDir string) *TalkServer {
	t := &TalkServer{
		rooms:  map[string]*talkRoom{},
		pw:     map[string]*talkPwEntry{},
		pwFile: filepath.Join(dataDir, "talk-rooms.json"),
	}
	if b, err := os.ReadFile(t.pwFile); err == nil {
		json.Unmarshal(b, &t.pw)
	}
	return t
}

// ---------- 房间密码(对齐原 rooms.json 语义:PBKDF2-SHA256 60k) ----------

func talkHash(password, salt string) string {
	dk, _ := pbkdf2.Key(sha256.New, password, []byte(salt), 60_000, 32)
	return hex.EncodeToString(dk)
}

func (t *TalkServer) setPassword(room, password string) {
	salt := make([]byte, 12)
	rand.Read(salt)
	t.pw[room] = &talkPwEntry{Salt: hex.EncodeToString(salt), Hash: talkHash(password, hex.EncodeToString(salt)), CreatedAt: time.Now().UnixMilli()}
	t.savePwLocked()
}

func (t *TalkServer) verifyPassword(room, password string) bool {
	e, ok := t.pw[room]
	if !ok {
		return true // 未设密码 = 开放房间
	}
	if password == "" {
		return false
	}
	return talkHash(password, e.Salt) == e.Hash
}

func (t *TalkServer) savePwLocked() {
	if t.saveTmr != nil {
		return
	}
	t.saveTmr = time.AfterFunc(500*time.Millisecond, func() {
		t.mu.Lock()
		t.saveTmr = nil
		b, _ := json.MarshalIndent(t.pw, "", "  ")
		t.mu.Unlock()
		tmp := t.pwFile + ".tmp"
		if os.WriteFile(tmp, b, 0644) == nil {
			os.Rename(tmp, t.pwFile)
		}
	})
}

// ---------- 帧编解码 ----------

type talkFrame struct {
	Ev   string            `json:"ev"`
	Args []json.RawMessage `json:"args"`
	Bins []int             `json:"bins,omitempty"`
	bins [][]byte          // 解析出的附件
}

func parseTalkFrame(data []byte) (*talkFrame, error) {
	if len(data) < 2 {
		return nil, fmt.Errorf("frame too short")
	}
	hl := int(binary.BigEndian.Uint16(data))
	if 2+hl > len(data) {
		return nil, fmt.Errorf("bad header len")
	}
	var f talkFrame
	if err := json.Unmarshal(data[2:2+hl], &f); err != nil {
		return nil, err
	}
	off := 2 + hl
	for _, bl := range f.Bins {
		if bl < 0 || off+bl > len(data) {
			return nil, fmt.Errorf("bad bin len")
		}
		f.bins = append(f.bins, data[off:off+bl])
		off += bl
	}
	return &f, nil
}

// buildTalkFrame serialises ev+args(+attachments). args 里已含 {"$bin":i} 占位。
func buildTalkFrame(ev string, args []json.RawMessage, bins [][]byte) []byte {
	f := talkFrame{Ev: ev, Args: args}
	for _, b := range bins {
		f.Bins = append(f.Bins, len(b))
	}
	hdr, _ := json.Marshal(f)
	total := 2 + len(hdr)
	for _, b := range bins {
		total += len(b)
	}
	out := make([]byte, 0, total)
	out = binary.BigEndian.AppendUint16(out, uint16(len(hdr)))
	out = append(out, hdr...)
	for _, b := range bins {
		out = append(out, b...)
	}
	return out
}

func jraw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// ---------- 广播 ----------

func (p *talkPeer) push(frame []byte) {
	select {
	case p.send <- frame:
	default: // 背压:慢客户端丢帧,不能拖垮整个房间
	}
}

// broadcastLocked sends to everyone in room except exceptID ("" = all).
func (t *TalkServer) broadcastLocked(room, exceptID string, frame []byte) {
	r := t.rooms[room]
	if r == nil {
		return
	}
	for id, p := range r.peers {
		if id == exceptID {
			continue
		}
		p.push(frame)
	}
}

// ---------- WS 主循环 ----------

func (t *TalkServer) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := talkUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(talkMaxFrame)

	t.mu.Lock()
	t.nextID++
	id := fmt.Sprintf("qt%08x", t.nextID)
	t.mu.Unlock()

	p := &talkPeer{id: id, conn: conn, send: make(chan []byte, 256)}

	// writer goroutine — conn 写不是并发安全的,全部经 send 走单写协程
	go func() {
		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()
		for {
			select {
			case frame, ok := <-p.send:
				if !ok {
					conn.WriteControl(websocket.CloseMessage, nil, time.Now().Add(time.Second))
					return
				}
				conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
				if conn.WriteMessage(websocket.BinaryMessage, frame) != nil {
					return
				}
			case <-ping.C:
				conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
				if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(15*time.Second)) != nil {
					return
				}
			}
		}
	}()

	// 告知客户端连接建立 + 分配的 id(shim 触发 'connect')
	p.push(buildTalkFrame("connect", []json.RawMessage{jraw(map[string]string{"id": id})}, nil))

	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		if mt != websocket.BinaryMessage {
			continue
		}
		f, err := parseTalkFrame(data)
		if err != nil {
			continue
		}
		t.dispatch(p, f)
	}

	// 断开清理
	if t.wt != nil {
		t.wt.DropPeer(p.id)
	}
	t.mu.Lock()
	room := p.room
	leftEmpty := false
	if room != "" {
		if r := t.rooms[room]; r != nil {
			delete(r.peers, p.id)
			if len(r.peers) == 0 {
				delete(t.rooms, room)
				leftEmpty = true
			} else {
				t.broadcastLocked(room, "", buildTalkFrame("peer-left",
					[]json.RawMessage{jraw(map[string]string{"id": p.id})}, nil))
			}
		}
	}
	p.closed = true
	close(p.send)
	t.mu.Unlock()
	if leftEmpty {
		go talkRoomActivity(room, 1, 0)
	}
	conn.Close()
}

// ---------- 事件分发(对齐原 server/index.js 全部事件) ----------

func (t *TalkServer) dispatch(p *talkPeer, f *talkFrame) {
	arg := func(i int, v any) bool {
		if i >= len(f.Args) {
			return false
		}
		return json.Unmarshal(f.Args[i], v) == nil
	}

	switch f.Ev {
	case "join":
		var req struct {
			Room        string `json:"room"`
			Name        string `json:"name"`
			Password    string `json:"password"`
			SetPassword string `json:"setPassword"`
		}
		if !arg(0, &req) {
			return
		}
		room := strings.ToUpper(strings.TrimSpace(req.Room))
		if len(room) > 12 {
			room = room[:12]
		}
		if room == "" {
			return
		}
		t.mu.Lock()
		if req.SetPassword != "" {
			if _, has := t.pw[room]; !has {
				t.setPassword(room, req.SetPassword)
			}
		}
		if !t.verifyPassword(room, req.Password) {
			reason := "needed"
			if req.Password != "" {
				reason = "wrong"
			}
			t.mu.Unlock()
			p.push(buildTalkFrame("auth-required",
				[]json.RawMessage{jraw(map[string]string{"room": room, "reason": reason})}, nil))
			return
		}
		// 允许换房:先离开旧房
		if p.room != "" && p.room != room {
			if old := t.rooms[p.room]; old != nil {
				delete(old.peers, p.id)
				if len(old.peers) == 0 {
					delete(t.rooms, p.room)
				} else {
					t.broadcastLocked(p.room, "", buildTalkFrame("peer-left",
						[]json.RawMessage{jraw(map[string]string{"id": p.id})}, nil))
				}
			}
		}
		p.room = room
		p.name = req.Name
		if p.name == "" {
			p.name = p.id[:6]
		}
		p.micOn, p.screenOn = false, false
		r := t.rooms[room]
		if r == nil {
			r = &talkRoom{peers: map[string]*talkPeer{}}
			t.rooms[room] = r
		}
		before := len(r.peers)
		r.peers[p.id] = p
		after := len(r.peers)
		_, hasPw := t.pw[room]
		// 现有成员列表(不含自己)
		list := []map[string]any{}
		for id, m := range r.peers {
			if id == p.id {
				continue
			}
			list = append(list, map[string]any{"id": id, "name": m.name, "micOn": m.micOn, "screenOn": m.screenOn})
		}
		p.push(buildTalkFrame("joined", []json.RawMessage{jraw(map[string]any{"room": room, "hasPassword": hasPw})}, nil))
		p.push(buildTalkFrame("peers", []json.RawMessage{jraw(map[string]any{"list": list})}, nil))
		t.broadcastLocked(room, p.id, buildTalkFrame("peer-joined",
			[]json.RawMessage{jraw(map[string]any{"id": p.id, "name": p.name, "micOn": false, "screenOn": false})}, nil))
		t.mu.Unlock()
		t.sendWTInfo(p)
		go talkRoomActivity(room, before, after)

	case "rename":
		var req struct {
			Name string `json:"name"`
		}
		if !arg(0, &req) || p.room == "" {
			return
		}
		clean := strings.TrimSpace(req.Name)
		if len(clean) > 24 {
			clean = clean[:24]
		}
		if clean == "" {
			clean = p.id[:6]
		}
		t.mu.Lock()
		p.name = clean
		t.broadcastLocked(p.room, p.id, buildTalkFrame("peer-renamed",
			[]json.RawMessage{jraw(map[string]string{"id": p.id, "name": clean})}, nil))
		t.mu.Unlock()

	case "voice", "video", "screen-audio":
		// 媒体中转:原样转发,发送者 id 插到 args[0]
		if p.room == "" {
			return
		}
		args := append([]json.RawMessage{jraw(p.id)}, f.Args...)
		frame := buildTalkFrame(f.Ev, args, f.bins)
		t.mu.Lock()
		t.broadcastLocked(p.room, p.id, frame)
		t.mu.Unlock()

	case "need-keyframe":
		if p.room == "" {
			return
		}
		t.mu.Lock()
		t.broadcastLocked(p.room, p.id, buildTalkFrame("need-keyframe", nil, nil))
		t.mu.Unlock()

	case "need-codec":
		var req struct {
			Wanted      string `json:"wanted"`
			AvoidString string `json:"avoidString"`
		}
		if !arg(0, &req) || p.room == "" {
			return
		}
		if req.Wanted == "" {
			req.Wanted = "h264"
		}
		out := map[string]string{"wanted": req.Wanted}
		if req.AvoidString != "" {
			out["avoidString"] = req.AvoidString
		}
		t.mu.Lock()
		t.broadcastLocked(p.room, p.id, buildTalkFrame("need-codec", []json.RawMessage{jraw(out)}, nil))
		t.mu.Unlock()

	case "codec-string-unsupported", "codec-unavailable":
		var req struct {
			Codec string `json:"codec"`
		}
		if !arg(0, &req) || req.Codec == "" || p.room == "" {
			return
		}
		t.mu.Lock()
		t.broadcastLocked(p.room, p.id, buildTalkFrame(f.Ev,
			[]json.RawMessage{jraw(map[string]string{"codec": req.Codec})}, nil))
		t.mu.Unlock()

	case "sharer-transport":
		var req struct {
			Mode string `json:"mode"`
		}
		if !arg(0, &req) || p.room == "" {
			return
		}
		mode := "tcp"
		if req.Mode == "wt" {
			mode = "wt"
		}
		t.mu.Lock()
		t.broadcastLocked(p.room, p.id, buildTalkFrame("sharer-transport",
			[]json.RawMessage{jraw(map[string]string{"mode": mode, "from": p.id})}, nil))
		t.mu.Unlock()

	case "state":
		var req struct {
			MicOn    bool `json:"micOn"`
			ScreenOn bool `json:"screenOn"`
		}
		if !arg(0, &req) || p.room == "" {
			return
		}
		t.mu.Lock()
		p.micOn, p.screenOn = req.MicOn, req.ScreenOn
		t.broadcastLocked(p.room, p.id, buildTalkFrame("peer-state",
			[]json.RawMessage{jraw(map[string]any{"id": p.id, "micOn": p.micOn, "screenOn": p.screenOn})}, nil))
		t.mu.Unlock()

	case "chat":
		var req struct {
			Text  string `json:"text"`
			Image string `json:"image"`
			Ts    int64  `json:"ts"`
		}
		if !arg(0, &req) || p.room == "" {
			return
		}
		if len(req.Text) > 2000 { // 500 字符,UTF-8 保守 x4
			req.Text = req.Text[:2000]
		}
		img := ""
		if strings.HasPrefix(req.Image, "data:image/") && len(req.Image) < 6_500_000 {
			img = req.Image
		}
		if req.Text == "" && img == "" {
			return
		}
		ts := req.Ts
		if ts == 0 {
			ts = time.Now().UnixMilli()
		}
		t.mu.Lock()
		name := p.name
		room := p.room
		t.broadcastLocked(p.room, "", buildTalkFrame("chat", []json.RawMessage{jraw(map[string]any{
			"from": p.id, "name": p.name, "text": req.Text, "image": img, "ts": ts,
		})}, nil))
		t.mu.Unlock()
		// 桥接:语音房文字 → 游戏内聊天(图片不转)
		if t.mgr != nil && req.Text != "" {
			go t.mgr.talkChatToGame(room, name, req.Text)
		}

	case "need-wt-token":
		t.sendWTInfo(p)
	}
}

// sendWTInfo issues a WT token and tells the client where to dial. No-op when
// the QUIC relay is disabled — the frontend then stays on pure WS.
func (t *TalkServer) sendWTInfo(p *talkPeer) {
	if t.wt == nil || p.room == "" {
		return
	}
	token, url, certHash := t.wt.IssueToken(p.id, p.room)
	p.push(buildTalkFrame("webtransport",
		[]json.RawMessage{jraw(map[string]string{"url": url, "token": token, "certHash": certHash})}, nil))
}

// handleTalkHealth 对齐原 GET /health
func (t *TalkServer) HandleHealth(w http.ResponseWriter, r *http.Request) {
	t.mu.Lock()
	nRooms := len(t.rooms)
	nPeers := 0
	for _, r := range t.rooms {
		nPeers += len(r.peers)
	}
	t.mu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true, "ts": time.Now().UnixMilli(), "rooms": nRooms, "peers": nPeers})
}

// HandleRooms returns per-room peer counts: {"C1FDDB54": 2, ...}
func (t *TalkServer) HandleRooms(w http.ResponseWriter, r *http.Request) {
	t.mu.Lock()
	out := map[string]int{}
	for name, room := range t.rooms {
		out[name] = len(room.peers)
	}
	t.mu.Unlock()
	writeJSON(w, 200, out)
}
