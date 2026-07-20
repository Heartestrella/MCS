package server

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ConsoleHub keeps a ring buffer of recent lines and fans out to websocket clients.
type ConsoleHub struct {
	mu      sync.Mutex
	history []string
	clients map[*websocket.Conn]chan string

	// 当次启动的完整日志（供 AI 分析）：保留开头 + 结尾，中间超出部分丢弃。
	// mod 加载报错集中在启动初期，崩溃堆栈在末尾，两头都要。
	launchHead    []string
	launchTail    []string
	launchDropped int

	// 当前下载/安装进度（供实例卡片进度条轮询），total=0 表示不确定进度
	dlLabel string
	dlDone  int64
	dlTotal int64

	// 结构化安装步骤（供下载管理页展示），并行步骤各自按名字更新
	steps      []InstallStep
	filesTotal int
	filesDone  int

	// 下载速度统计：所有并发下载协程通过 AddBytes 汇总字节数，
	// 用近几秒的采样窗口算瞬时速度（B/s）
	dlBytes   int64
	dlSamples []dlSample
}

// InstallStep 是安装任务里的一个步骤，Status: pending / running / done / error。
type InstallStep struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Done   int64  `json:"done,omitempty"`
	Total  int64  `json:"total,omitempty"`
}

type dlSample struct {
	at    time.Time
	bytes int64
}

const (
	launchHeadMax = 2000
	launchTailMax = 3000
)

// ResetLaunchLog starts a fresh per-launch log capture. Call on every start.
func (h *ConsoleHub) ResetLaunchLog() {
	h.mu.Lock()
	h.launchHead, h.launchTail, h.launchDropped = nil, nil, 0
	h.mu.Unlock()
}

// LaunchLog returns the full log of the current/last launch (head + tail,
// with a marker for dropped middle lines).
func (h *ConsoleHub) LaunchLog() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.launchHead)+len(h.launchTail)+1)
	out = append(out, h.launchHead...)
	if h.launchDropped > 0 {
		out = append(out, fmt.Sprintf("……（中间省略 %d 行）……", h.launchDropped))
	}
	out = append(out, h.launchTail...)
	return out
}

// SetProgress records the current download progress shown on the instance card.
// label 与某个步骤同名时，同时把该步骤置为 running 并更新其进度，
// 这样各下载函数不用改签名就能把进度汇报到下载管理页对应的步骤上。
func (h *ConsoleHub) SetProgress(label string, done, total int64) {
	h.mu.Lock()
	h.dlLabel, h.dlDone, h.dlTotal = label, done, total
	for i := range h.steps {
		if h.steps[i].Name == label && h.steps[i].Status != "done" && h.steps[i].Status != "error" {
			h.steps[i].Status = "running"
			h.steps[i].Done, h.steps[i].Total = done, total
			break
		}
	}
	h.mu.Unlock()
}

func (h *ConsoleHub) ClearProgress() {
	h.mu.Lock()
	h.dlLabel, h.dlDone, h.dlTotal = "", 0, 0
	h.dlBytes, h.dlSamples = 0, nil
	h.steps = nil
	h.filesTotal, h.filesDone = 0, 0
	h.mu.Unlock()
}

// SetSteps resets the structured install step list (all pending).
func (h *ConsoleHub) SetSteps(names ...string) {
	h.mu.Lock()
	h.steps = make([]InstallStep, len(names))
	for i, n := range names {
		h.steps[i] = InstallStep{Name: n, Status: "pending"}
	}
	h.mu.Unlock()
}

// AddSteps appends steps to the existing list.
// 早期步骤进行中才知道后续步骤时用（如整合包解析清单后按加载器类型追加）。
func (h *ConsoleHub) AddSteps(names ...string) {
	h.mu.Lock()
	for _, n := range names {
		h.steps = append(h.steps, InstallStep{Name: n, Status: "pending"})
	}
	h.mu.Unlock()
}

func (h *ConsoleHub) setStepStatus(name, status string) {
	h.mu.Lock()
	for i := range h.steps {
		if h.steps[i].Name == name {
			h.steps[i].Status = status
			if status == "done" && h.steps[i].Total > 0 {
				h.steps[i].Done = h.steps[i].Total
			}
			break
		}
	}
	h.mu.Unlock()
}

func (h *ConsoleHub) StepRun(name string)  { h.setStepStatus(name, "running") }
func (h *ConsoleHub) StepDone(name string) { h.setStepStatus(name, "done") }
func (h *ConsoleHub) StepFail(name string) { h.setStepStatus(name, "error") }

// Steps returns a copy of the current install steps.
func (h *ConsoleHub) Steps() []InstallStep {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]InstallStep, len(h.steps))
	copy(out, h.steps)
	return out
}

// AddFilesTotal / FileDone / FilesLeft: 下载管理页"剩余文件"计数。
func (h *ConsoleHub) AddFilesTotal(n int) {
	h.mu.Lock()
	h.filesTotal += n
	h.mu.Unlock()
}

func (h *ConsoleHub) FileDone() {
	h.mu.Lock()
	h.filesDone++
	h.mu.Unlock()
}

func (h *ConsoleHub) FilesLeft() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.filesTotal <= h.filesDone {
		return 0
	}
	return h.filesTotal - h.filesDone
}

func (h *ConsoleHub) Progress() (label string, done, total int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dlLabel, h.dlDone, h.dlTotal
}

// speedWindow: 瞬时速度的采样窗口。太短数字乱跳，太长反应迟钝。
const speedWindow = 5 * time.Second

// AddBytes accumulates downloaded bytes from any goroutine for speed stats.
// 采样按 100ms 分桶，窗口内最多几十个点，锁开销可忽略。
func (h *ConsoleHub) AddBytes(n int64) {
	now := time.Now()
	h.mu.Lock()
	h.dlBytes += n
	if k := len(h.dlSamples); k == 0 || now.Sub(h.dlSamples[k-1].at) >= 100*time.Millisecond {
		h.dlSamples = append(h.dlSamples, dlSample{at: now, bytes: h.dlBytes})
		cut := 0
		for cut < len(h.dlSamples)-1 && now.Sub(h.dlSamples[cut+1].at) > speedWindow {
			cut++
		}
		h.dlSamples = h.dlSamples[cut:]
	}
	h.mu.Unlock()
}

// Speed returns the current download speed in bytes/sec (0 if idle or stalled).
func (h *ConsoleHub) Speed() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.dlSamples) < 2 {
		return 0
	}
	first, last := h.dlSamples[0], h.dlSamples[len(h.dlSamples)-1]
	// 最近一次采样已是 3 秒前 → 下载停滞，速度归零
	if time.Since(last.at) > 3*time.Second {
		return 0
	}
	dur := last.at.Sub(first.at).Seconds()
	if dur <= 0 {
		return 0
	}
	return int64(float64(last.bytes-first.bytes) / dur)
}

const historyMax = 500

func NewConsoleHub() *ConsoleHub {
	return &ConsoleHub{clients: map[*websocket.Conn]chan string{}}
}

func (h *ConsoleHub) Broadcast(line string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.history = append(h.history, line)
	if len(h.history) > historyMax {
		h.history = h.history[len(h.history)-historyMax:]
	}
	if len(h.launchHead) < launchHeadMax {
		h.launchHead = append(h.launchHead, line)
	} else {
		h.launchTail = append(h.launchTail, line)
		if len(h.launchTail) > launchTailMax {
			over := len(h.launchTail) - launchTailMax
			h.launchDropped += over
			h.launchTail = h.launchTail[over:]
		}
	}
	for _, ch := range h.clients {
		select {
		case ch <- line:
		default: // slow client, drop line
		}
	}
}

// Recent returns up to n most recent console lines.
func (h *ConsoleHub) Recent(n int) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	start := len(h.history) - n
	if start < 0 {
		start = 0
	}
	out := make([]string, len(h.history)-start)
	copy(out, h.history[start:])
	return out
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true }, // panel binds to 127.0.0.1 only
}

// Serve upgrades the connection, replays history, then streams new lines.
// Incoming text messages are treated as console commands via sendCmd.
func (h *ConsoleHub) Serve(w http.ResponseWriter, r *http.Request, sendCmd func(string) error) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ch := make(chan string, 256)

	h.mu.Lock()
	replay := make([]string, len(h.history))
	copy(replay, h.history)
	h.clients[conn] = ch
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.clients, conn)
		h.mu.Unlock()
		conn.Close()
	}()

	for _, line := range replay {
		if conn.WriteMessage(websocket.TextMessage, []byte(line)) != nil {
			return
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if len(msg) > 0 && sendCmd != nil {
				sendCmd(string(msg))
			}
		}
	}()

	for {
		select {
		case line := <-ch:
			if conn.WriteMessage(websocket.TextMessage, []byte(line)) != nil {
				return
			}
		case <-done:
			return
		}
	}
}
