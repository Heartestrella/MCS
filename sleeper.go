package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// ===== 空服自动休眠 / 玩家连接自动唤醒 =====
// 空服超时后优雅停服释放内存，在原端口挂一个极轻量的 MC 协议监听：
// 服务器列表 ping 显示「待机中」MOTD；玩家点进入 → 触发唤醒并提示稍后重试。

var sleepAfter = func() time.Duration {
	if v, err := time.ParseDuration(os.Getenv("MCS_SLEEP_AFTER")); err == nil && v >= 30*time.Second {
		return v
	}
	return 10 * time.Minute
}()

var (
	sleepMu         sync.Mutex
	sleepLn         = map[string]*net.Listener{}
	sleepLastActive = map[string]time.Time{}
)

// startSleeper monitors running instances and puts empty ones to standby.
func (m *Manager) startSleeper() {
	go func() {
		for range time.Tick(30 * time.Second) {
			m.mu.Lock()
			var cands []*Instance
			now := time.Now()
			for id, in := range m.insts {
				rs, ok := m.rt[id]
				if !ok || rs.status != "running" {
					sleepMu.Lock()
					delete(sleepLastActive, id)
					sleepMu.Unlock()
					continue
				}
				sleepMu.Lock()
				if len(rs.players) > 0 {
					sleepLastActive[id] = now
				} else if _, seen := sleepLastActive[id]; !seen {
					sleepLastActive[id] = now // 刚发现它在跑，从现在计时
				}
				idle := now.Sub(sleepLastActive[id])
				sleepMu.Unlock()
				if in.AutoSleep && idle >= sleepAfter {
					cands = append(cands, in)
				}
			}
			m.mu.Unlock()

			for _, in := range cands {
				m.goToSleep(in, false)
			}
		}
	}()
}

// goToSleep gracefully stops the server then starts the wake listener.
func (m *Manager) goToSleep(in *Instance, manual bool) {
	rs := m.getRTSafe(in.ID)
	if manual {
		rs.console.Broadcast("[MCS] 手动进入待机：关闭服务器释放内存（玩家点击进入会自动唤醒）")
		m.addActivity("blue", fmt.Sprintf("<b>%s</b> 手动进入待机", in.Name))
	} else {
		rs.console.Broadcast(fmt.Sprintf("[MCS] 已经 %d 分钟没有玩家了，自动进入待机释放内存（有人进入会自动唤醒）", int(sleepAfter.Minutes())))
		m.addActivity("blue", fmt.Sprintf("<b>%s</b> 空服自动进入待机", in.Name))
	}
	if err := m.stopInstance(in); err != nil {
		return
	}
	// 等完全停止（最多 90 秒）
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		m.mu.Lock()
		st := rs.status
		m.mu.Unlock()
		if st == "stopped" || st == "error" {
			break
		}
	}
	m.mu.Lock()
	if rs.status != "stopped" {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	m.startWakeListener(in, rs)
}

// startWakeListener binds the instance port and answers MC pings while asleep.
func (m *Manager) startWakeListener(in *Instance, rs *runtimeState) {
	// 刚停服时端口可能尚未完全释放，重试最多 30 秒
	var ln net.Listener
	var err error
	for i := 0; i < 10; i++ {
		ln, err = net.Listen("tcp", fmt.Sprintf(":%d", in.Port))
		if err == nil {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		rs.console.Broadcast("[MCS] 待机监听启动失败: " + err.Error())
		m.mu.Lock()
		in.Sleeping = false
		m.save()
		m.mu.Unlock()
		return
	}
	sleepMu.Lock()
	sleepLn[in.ID] = &ln
	sleepMu.Unlock()
	m.mu.Lock()
	rs.status = "sleeping"
	in.Sleeping = true
	m.save()
	m.mu.Unlock()
	rs.console.Broadcast("[MCS] 已进入待机：内存已释放，玩家在服务器列表仍能看到本服，点进入即自动唤醒")

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed = 唤醒或停用
			}
			go m.handleSleepConn(in, conn)
		}
	}()
}

// stopWakeListener closes the standby listener if present; returns true if it was active.
func (m *Manager) stopWakeListener(id string) bool {
	sleepMu.Lock()
	lnp, ok := sleepLn[id]
	delete(sleepLn, id)
	delete(sleepLastActive, id)
	sleepMu.Unlock()
	if ok && lnp != nil {
		(*lnp).Close()
	}
	m.mu.Lock()
	if in, exists := m.insts[id]; exists && in.Sleeping {
		in.Sleeping = false
		m.save()
	}
	m.mu.Unlock()
	return ok
}

// resumeSleepers re-arms wake listeners for instances that were sleeping when
// the panel exited (otherwise their port answers nothing → connection refused).
func (m *Manager) resumeSleepers() {
	m.mu.Lock()
	var list []*Instance
	for _, in := range m.insts {
		// AutoStart 的实例已在启动流程里，不再挂待机监听
		if in.Sleeping && m.getRT(in.ID).status == "stopped" && !in.AutoStart {
			list = append(list, in)
		} else if in.Sleeping && in.AutoStart {
			in.Sleeping = false
			m.save()
		}
	}
	m.mu.Unlock()
	for _, in := range list {
		rs := m.getRTSafe(in.ID)
		rs.console.Broadcast("[MCS] 面板重启，恢复待机唤醒监听")
		m.startWakeListener(in, rs)
	}
}

// handleSleepConn speaks just enough Minecraft protocol to show a MOTD and
// catch join attempts.
func (m *Manager) handleSleepConn(in *Instance, conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	// handshake
	pkt, err := readPacket(conn)
	if err != nil || len(pkt) < 1 {
		return
	}
	rd := &pktReader{b: pkt}
	if id, _ := rd.varint(); id != 0x00 {
		return
	}
	rd.varint() // protocol version
	rd.str()    // server address
	rd.skip(2)  // port
	next, _ := rd.varint()

	switch next {
	case 1: // status
		if p, err := readPacket(conn); err != nil || len(p) == 0 {
			return
		}
		desc := fmt.Sprintf("§e%s §7(待机中)\n§a点击进入即可唤醒服务器", in.Name)
		status := map[string]any{
			"version":     map[string]any{"name": "待机中", "protocol": -1},
			"players":     map[string]any{"max": 0, "online": 0},
			"description": map[string]any{"text": desc},
		}
		sb, _ := json.Marshal(status)
		writePacket(conn, buildPacket(0x00, encStr(string(sb))))
		// ping/pong
		if p, err := readPacket(conn); err == nil && len(p) >= 9 {
			writePacket(conn, buildPacket(0x01, p[1:]))
		}
	case 2: // login → 唤醒
		msg := map[string]any{"text": "§a服务器正在唤醒，大约 30 秒后再进一次就好啦", "color": "green"}
		mb, _ := json.Marshal(msg)
		writePacket(conn, buildPacket(0x00, encStr(string(mb))))
		go m.wakeUp(in)
	}
}

// wakeUp closes the listener and restarts the real server (idempotent).
func (m *Manager) wakeUp(in *Instance) {
	if !m.stopWakeListener(in.ID) {
		return // 已有并发唤醒
	}
	m.mu.Lock()
	rs := m.getRT(in.ID)
	if rs.status == "sleeping" {
		rs.status = "stopped"
	}
	m.mu.Unlock()
	rs.console.Broadcast("[MCS] 有玩家敲门，正在唤醒服务器 ...")
	m.addActivity("green", fmt.Sprintf("<b>%s</b> 有玩家连接，自动唤醒", in.Name))
	if err := m.startInstance(in); err != nil {
		rs.console.Broadcast("[MCS] 自动唤醒失败: " + err.Error())
	}
}

// ===== 极简 MC 协议编解码 =====

func readVarint(r io.Reader) (int, error) {
	var n, shift uint
	buf := make([]byte, 1)
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			return 0, err
		}
		n |= uint(buf[0]&0x7f) << shift
		if buf[0]&0x80 == 0 {
			return int(n), nil
		}
		shift += 7
		if shift > 35 {
			return 0, fmt.Errorf("varint too long")
		}
	}
}

func encVarint(v int) []byte {
	var out []byte
	u := uint32(v)
	for {
		b := byte(u & 0x7f)
		u >>= 7
		if u != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if u == 0 {
			return out
		}
	}
}

func encStr(s string) []byte {
	return append(encVarint(len(s)), s...)
}

// readPacket reads one length-prefixed packet (≤32KB).
func readPacket(r io.Reader) ([]byte, error) {
	n, err := readVarint(r)
	if err != nil {
		return nil, err
	}
	if n <= 0 || n > 32*1024 {
		return nil, fmt.Errorf("bad packet length %d", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

func buildPacket(id int, payload []byte) []byte {
	body := append(encVarint(id), payload...)
	return append(encVarint(len(body)), body...)
}

func writePacket(w io.Writer, pkt []byte) error {
	_, err := w.Write(pkt)
	return err
}

type pktReader struct {
	b   []byte
	pos int
}

func (r *pktReader) varint() (int, error) {
	var n, shift uint
	for {
		if r.pos >= len(r.b) {
			return 0, io.EOF
		}
		c := r.b[r.pos]
		r.pos++
		n |= uint(c&0x7f) << shift
		if c&0x80 == 0 {
			return int(n), nil
		}
		shift += 7
		if shift > 35 {
			return 0, fmt.Errorf("varint too long")
		}
	}
}

func (r *pktReader) str() (string, error) {
	n, err := r.varint()
	if err != nil || n < 0 || r.pos+n > len(r.b) {
		return "", io.EOF
	}
	s := string(r.b[r.pos : r.pos+n])
	r.pos += n
	return s, nil
}

func (r *pktReader) skip(n int) { r.pos += n }
