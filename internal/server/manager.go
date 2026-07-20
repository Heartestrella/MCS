package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/shirou/gopsutil/v3/process"
	"golang.org/x/text/encoding/simplifiedchinese"
)

type Instance struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Type           string    `json:"type"`    // paper / fabric / forge / neoforge / custom / generic
	Version        string    `json:"version"` // e.g. 1.21.4
	Port           int       `json:"port"`
	MemoryMB       int       `json:"memoryMB"`
	JarFile        string    `json:"jarFile"`
	JavaPath       string    `json:"javaPath"`
	Dir            string    `json:"dir,omitempty"` // 自定义部署路径（空=data/instances/<id>）
	AutoRestart    bool      `json:"autoRestart"`
	AutoSleep      bool      `json:"autoSleep"`
	Sleeping       bool      `json:"sleeping,omitempty"` // 持久化待机状态，面板重启后恢复唤醒监听
	AutoStart      bool      `json:"autoStart"`
	TalkInvite     bool      `json:"talkInvite,omitempty"` // 玩家进服自动私发语音房直达地址
	UpnpMapped     bool      `json:"upnpMapped,omitempty"` // UPnP 映射已开启（持久化，用于地址中心与 IP 变化监控）
	OptimizedFlags bool      `json:"optimizedFlags"`
	ExecCmd        string    `json:"execCmd,omitempty"`    // generic: 启动命令行
	StopCmd        string    `json:"stopCmd,omitempty"`    // generic: 停止指令（空=直接结束进程）
	WorkDir        string    `json:"workDir,omitempty"`    // generic: 运行根目录（空=实例目录）
	SteamAppID     int       `json:"steamAppId,omitempty"` // generic: SteamCMD 应用 ID
	CreatedAt      time.Time `json:"createdAt"`
	LastActive     time.Time `json:"lastActive"`

	// runtime, not persisted
	Status     string   `json:"status"` // stopped / starting / running / stopping / downloading / error
	Players    int      `json:"players"`
	PlayerList []string `json:"playerList,omitempty"`
	UptimeSec  int      `json:"uptimeSec,omitempty"`
	CPUPct     float64  `json:"cpuPct,omitempty"`
	MemUsedMB  int      `json:"memUsedMB,omitempty"`
	Error      string   `json:"error,omitempty"`
	NeedJava   int      `json:"needJava,omitempty"` // 启动失败需要的 Java 大版本，前端提示一键安装
	DlLabel    string   `json:"dlLabel,omitempty"`  // downloading 时的进度描述
	DlDone     int64    `json:"dlDone,omitempty"`
	DlTotal    int64    `json:"dlTotal,omitempty"`
	DlSpeed    int64    `json:"dlSpeed,omitempty"` // 当前下载速度 B/s
	TPS        float64  `json:"tps,omitempty"`     // 1m TPS（Paper 系运行中）
}

type runtimeState struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	console   *ConsoleHub
	status    string
	players   map[string]bool
	errMsg    string
	startedAt time.Time
	cpuPct    float64
	memUsedMB int
	crashes   []time.Time // 近期崩溃时间，用于熔断
	diag      string      // 启动失败诊断（原因+建议）
	tps       [3]float64  // 1m/5m/15m TPS（Paper 系）
	tpsAt     time.Time   // 最近一次 TPS 采样时间
	tpsSilent bool        // 面板静默发起的 tps 查询，响应行不进控制台
	needJava  int         // 启动失败检测到需要的 Java 大版本（0=无）
}

type Activity struct {
	Icon string    `json:"icon"` // green / blue / orange
	Text string    `json:"text"`
	Time time.Time `json:"time"`
}

type Manager struct {
	mu       sync.Mutex
	dataDir  string
	insts    map[string]*Instance
	rt       map[string]*runtimeState
	activity []Activity
	dirs     sync.Map // instID -> 自定义部署路径（避免 instDir 加锁）
}

func NewManager(dataDir string) (*Manager, error) {
	m := &Manager{
		dataDir: dataDir,
		insts:   map[string]*Instance{},
		rt:      map[string]*runtimeState{},
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	m.applyProxyConfig(m.loadProxyConfig())
	m.startAutoBackup()
	m.startScheduledRestart()
	m.startTimedCmds()
	m.startStatsSampler()
	m.startSleeper()
	m.startTPSSampler()
	m.startIPWatcher()
	m.autoStartInstances()
	m.resumeSleepers()
	go m.cleanTrash()
	go m.sampleProcStats()
	return m, nil
}

// sampleProcStats periodically samples CPU/memory of running server processes
// (including children — Forge's run.bat spawns java as a child).
func (m *Manager) sampleProcStats() {
	for range time.Tick(3 * time.Second) {
		type target struct {
			rs  *runtimeState
			pid int32
			id  string
		}
		m.mu.Lock()
		var ts []target
		for id, rs := range m.rt {
			if rs.cmd != nil && rs.cmd.Process != nil && (rs.status == "running" || rs.status == "starting") {
				ts = append(ts, target{rs, int32(rs.cmd.Process.Pid), id})
			}
		}
		m.mu.Unlock()

		for _, t := range ts {
			cpuSum, memSum := procTreeStats(t.pid)
			m.mu.Lock()
			t.rs.cpuPct = cpuSum
			t.rs.memUsedMB = memSum
			m.mu.Unlock()
			m.healthRecordPerf(t.id, cpuSum, memSum)
		}
	}
}

// procTreeStats returns total CPU% and RSS(MB) of a process and its children.
func procTreeStats(pid int32) (float64, int) {
	p, err := process.NewProcess(pid)
	if err != nil {
		return 0, 0
	}
	procs := []*process.Process{p}
	if kids, err := p.Children(); err == nil {
		procs = append(procs, kids...)
	}
	var cpuSum float64
	var memSum uint64
	for _, pr := range procs {
		if c, err := pr.CPUPercent(); err == nil {
			cpuSum += c
		}
		if mi, err := pr.MemoryInfo(); err == nil && mi != nil {
			memSum += mi.RSS
		}
	}
	// CPUPercent 是所有核累计，换算成整机百分比
	return cpuSum / float64(runtime.NumCPU()), int(memSum / 1024 / 1024)
}

func (m *Manager) metaPath() string { return filepath.Join(m.dataDir, "instances.json") }

func (m *Manager) instDir(id string) string {
	if v, ok := m.dirs.Load(id); ok {
		return v.(string)
	}
	return filepath.Join(m.dataDir, "instances", id)
}

func (m *Manager) load() error {
	b, err := os.ReadFile(m.metaPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var list []*Instance
	if err := json.Unmarshal(b, &list); err != nil {
		return err
	}
	for _, in := range list {
		in.Status = "stopped"
		in.Players = 0
		m.insts[in.ID] = in
		if in.Dir != "" {
			m.dirs.Store(in.ID, in.Dir)
		}
	}
	return nil
}

// save persists metadata; caller must hold m.mu.
func (m *Manager) save() {
	list := make([]*Instance, 0, len(m.insts))
	for _, in := range m.insts {
		list = append(list, in)
	}
	b, _ := json.MarshalIndent(list, "", "  ")
	if err := os.WriteFile(m.metaPath(), b, 0644); err != nil {
		log.Printf("save instances: %v", err)
	}
}

func (m *Manager) addActivity(icon, text string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activity = append([]Activity{{Icon: icon, Text: text, Time: time.Now()}}, m.activity...)
	if len(m.activity) > 50 {
		m.activity = m.activity[:50]
	}
}

func (m *Manager) getRT(id string) *runtimeState {
	rs, ok := m.rt[id]
	if !ok {
		rs = &runtimeState{status: "stopped", console: NewConsoleHub(), players: map[string]bool{}}
		m.rt[id] = rs
	}
	return rs
}

func (m *Manager) snapshot(in *Instance) Instance {
	cp := *in
	rs := m.getRT(in.ID)
	cp.Status = rs.status
	cp.Players = len(rs.players)
	if len(rs.players) > 0 {
		cp.PlayerList = make([]string, 0, len(rs.players))
		for p := range rs.players {
			cp.PlayerList = append(cp.PlayerList, p)
		}
		sort.Strings(cp.PlayerList)
	}
	cp.Error = rs.errMsg
	if rs.status == "error" {
		cp.NeedJava = rs.needJava
	}
	if rs.status == "downloading" || rs.status == "starting" {
		cp.DlLabel, cp.DlDone, cp.DlTotal = rs.console.Progress()
		cp.DlSpeed = rs.console.Speed()
	}
	if (rs.status == "running" || rs.status == "starting") && !rs.startedAt.IsZero() {
		cp.UptimeSec = int(time.Since(rs.startedAt).Seconds())
		cp.CPUPct = rs.cpuPct
		cp.MemUsedMB = rs.memUsedMB
		if rs.status == "running" && time.Since(rs.tpsAt) < 2*time.Minute {
			cp.TPS = rs.tps[0]
		}
	}
	return cp
}

var (
	reJoin  = regexp.MustCompile(`\]: (\w+) joined the game`)
	reLeave = regexp.MustCompile(`\]: (\w+) left the game`)
	reDone  = regexp.MustCompile(`\]: Done \(`)
	reAchv  = regexp.MustCompile(`\]: (\w+) has made the advancement \[(.+)\]`)
)

func (m *Manager) startInstance(in *Instance) error {
	// 待机中手动/自动启动：先收掉占端口的唤醒监听
	m.mu.Lock()
	if rs0 := m.getRT(in.ID); rs0.status == "sleeping" {
		rs0.status = "stopped"
		m.mu.Unlock()
		m.stopWakeListener(in.ID)
	} else {
		m.mu.Unlock()
	}
	m.mu.Lock()
	rs := m.getRT(in.ID)
	if rs.status == "running" || rs.status == "starting" {
		m.mu.Unlock()
		return fmt.Errorf("实例已在运行")
	}
	if rs.status == "downloading" {
		m.mu.Unlock()
		return fmt.Errorf("正在下载核心，请稍候")
	}
	// 端口冲突预警：其他运行中的实例占用同端口
	for oid, other := range m.insts {
		if oid == in.ID || other.Port != in.Port {
			continue
		}
		if ors, ok := m.rt[oid]; ok && (ors.status == "running" || ors.status == "starting") {
			m.mu.Unlock()
			return fmt.Errorf("端口 %d 已被「%s」占用，请在设置里改端口", in.Port, other.Name)
		}
	}

	dir := m.instDir(in.ID)
	if in.Type == "generic" {
		if in.ExecCmd == "" {
			m.mu.Unlock()
			return fmt.Errorf("未配置启动命令")
		}
	} else {
		jar := filepath.Join(dir, in.JarFile)
		if in.JarFile == "" {
			m.mu.Unlock()
			return fmt.Errorf("服务端核心还未就绪")
		}
		if _, err := os.Stat(jar); err != nil {
			m.mu.Unlock()
			return fmt.Errorf("服务端核心不存在: %s", in.JarFile)
		}
	}
	rs.status = "starting"
	rs.errMsg = ""
	rs.diag = ""
	rs.needJava = 0
	rs.console.ClearProgress()
	rs.console.ResetLaunchLog()
	m.mu.Unlock()

	go m.launch(in, rs, dir)
	return nil
}

func (m *Manager) launch(in *Instance, rs *runtimeState, dir string) {
	fail := func(msg string) {
		m.mu.Lock()
		rs.status = "error"
		rs.errMsg = msg
		m.mu.Unlock()
		rs.console.Broadcast("[MCS] " + msg)
	}

	var cmd *exec.Cmd
	mem := in.MemoryMB
	if mem <= 0 {
		mem = 2048
	}
	if in.Type == "generic" {
		// 通用服务器：平台 shell 执行任意命令行（SteamCMD 装的游戏等）
		cmd = shellCommand(in.ExecCmd)
	} else {
		javaPath := in.JavaPath
		if javaPath == "" {
			var err error
			javaPath, err = m.ensureJavaFor(in.Version, rs.console)
			if err != nil {
				fail(err.Error())
				return
			}
		}
		ext := filepath.Ext(in.JarFile)
		if strings.EqualFold(ext, ".bat") || strings.EqualFold(ext, ".sh") {
			// Forge/NeoForge 新版：官方 run.bat / run.sh 引导（内存参数写入 user_jvm_args.txt）
			var jvmArgs string
			if in.OptimizedFlags {
				jvmArgs = strings.Join(aikarFlags(mem), "\n") + "\n-Dfile.encoding=UTF-8\n-Dstdout.encoding=UTF-8\n-Dstderr.encoding=UTF-8\n"
			} else {
				jvmArgs = fmt.Sprintf("-Xms%dM\n-Xmx%dM\n-Dfile.encoding=UTF-8\n-Dstdout.encoding=UTF-8\n-Dstderr.encoding=UTF-8\n", mem/2, mem)
			}
			os.WriteFile(filepath.Join(dir, "user_jvm_args.txt"), []byte(jvmArgs), 0644)
			// 绝对路径启动，避免 cmd 不搜索当前目录（NoDefaultCurrentDirectoryInExePath）导致找不到 run.bat
			script := filepath.Join(dir, in.JarFile)
			if strings.EqualFold(ext, ".bat") {
				cmd = exec.Command("cmd", "/c", script, "nogui")
			} else {
				cmd = exec.Command("sh", script, "nogui")
			}
			// run 脚本用 PATH 里的 java（不认 JAVA_HOME），把托管 Java 的 bin 排最前
			javaBinDir := filepath.Dir(javaPath)
			cmd.Env = append(os.Environ(),
				"PATH="+javaBinDir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"JAVA_HOME="+filepath.Dir(javaBinDir))
		} else {
			var args []string
			if in.OptimizedFlags {
				args = aikarFlags(mem)
			} else {
				args = []string{
					fmt.Sprintf("-Xms%dM", mem/2),
					fmt.Sprintf("-Xmx%dM", mem),
				}
			}
			args = append(args,
				"-Dfile.encoding=UTF-8",
				"-Dstdout.encoding=UTF-8",
				"-Dstderr.encoding=UTF-8",
				"-Dsun.stdout.encoding=UTF-8",
				"-Dsun.stderr.encoding=UTF-8",
				"-jar", in.JarFile, "--nogui",
			)
			cmd = exec.Command(javaPath, args...)
		}
	}
	cmd.Dir = dir
	if in.Type == "generic" && in.WorkDir != "" {
		cmd.Dir = in.WorkDir
	}
	hideWindow(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		fail("启动失败: " + err.Error())
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fail("启动失败: " + err.Error())
		return
	}
	cmd.Stderr = cmd.Stdout // StdoutPipe 已把 Stdout 设为管道写端，stderr 合流

	if err := cmd.Start(); err != nil {
		fail("启动失败: " + err.Error())
		return
	}
	assignToJob(cmd) // 面板退出时自动结束服务器，防止孤儿进程占用存档锁

	m.mu.Lock()
	rs.cmd = cmd
	rs.stdin = stdin
	rs.players = map[string]bool{}
	rs.startedAt = time.Now()
	if in.Type == "generic" {
		rs.status = "running" // 通用服务器没有统一的“启动完成”标志，进程起来即视为运行
	}
	in.LastActive = time.Now()
	m.save()
	m.mu.Unlock()

	if in.Type == "generic" {
		rs.console.Broadcast(fmt.Sprintf("[MCS] 正在启动 %s（命令: %s）...", in.Name, in.ExecCmd))
	} else {
		rs.console.Broadcast(fmt.Sprintf("[MCS] 正在启动 %s (内存: %dMB)...", in.Name, mem))
	}
	m.addActivity("green", fmt.Sprintf("<b>%s</b> 正在启动", in.Name))

	go m.pipeConsole(in, rs, stdout)
	go func() {
		err := cmd.Wait()
		m.mu.Lock()
		wasRunning := rs.status == "running"
		// 启动阶段就退出视为失败（部分服务端如 Paper 绑定失败也返回退出码 0）
		if rs.status == "starting" && (err != nil || rs.diag != "" || rs.needJava > 0) {
			rs.status = "error"
			if rs.needJava > 0 && rs.diag == "" {
				rs.errMsg = fmt.Sprintf("Java 版本不匹配，需要 Java %d", rs.needJava)
			} else if rs.diag != "" {
				rs.errMsg = rs.diag
			} else {
				rs.errMsg = "启动失败，请查看控制台日志"
			}
		} else {
			rs.status = "stopped"
		}
		rs.diag = ""
		rs.players = map[string]bool{}
		rs.cmd = nil
		rs.stdin = nil
		autoRestart := in.AutoRestart && wasRunning
		if autoRestart {
			// 熔断：10 分钟内崩溃 3 次就放弃
			cut := time.Now().Add(-10 * time.Minute)
			kept := rs.crashes[:0]
			for _, t := range rs.crashes {
				if t.After(cut) {
					kept = append(kept, t)
				}
			}
			rs.crashes = append(kept, time.Now())
			if len(rs.crashes) > 3 {
				autoRestart = false
			}
		}
		m.mu.Unlock()

		rs.console.Broadcast("[MCS] 服务器已停止")
		m.addActivity("blue", fmt.Sprintf("<b>%s</b> 已停止", in.Name))
		m.statsOnStop(in.ID)

		if !wasRunning || !in.AutoRestart {
			return
		}
		if !autoRestart {
			rs.console.Broadcast("[MCS] 10 分钟内崩溃超过 3 次，已暂停自动重启，请检查日志")
			m.addActivity("orange", fmt.Sprintf("<b>%s</b> 频繁崩溃，自动重启已熔断", in.Name))
			m.notify("服务器频繁崩溃", fmt.Sprintf("世界「%s」10 分钟内崩溃超过 3 次，自动重启已暂停，请查看控制台日志排查原因。", in.Name))
			return
		}
		rs.console.Broadcast("[MCS] 检测到服务器意外退出，5 秒后自动重启 ...")
		m.addActivity("orange", fmt.Sprintf("<b>%s</b> 意外退出，正在自动重启", in.Name))
		time.Sleep(5 * time.Second)
		if err := m.startInstance(in); err != nil {
			rs.console.Broadcast("[MCS] 自动重启失败: " + err.Error())
		}
	}()
}

// toUTF8 fixes mojibake: if a console line is not valid UTF-8, decode it as GBK
// (Windows Java defaults to the ANSI code page for stdout in some setups).
func toUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	if dec, err := simplifiedchinese.GBK.NewDecoder().Bytes(b); err == nil {
		return string(dec)
	}
	return string(b)
}

var (
	reANSI  = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]|\x1b\][^\a]*\a?`)
	reMCFmt = regexp.MustCompile(`§[0-9a-fk-orx]`)
)

// cleanLine strips ANSI escape sequences and Minecraft § format codes that
// render as garbage in the web console.
func cleanLine(s string) string {
	s = reANSI.ReplaceAllString(s, "")
	s = reMCFmt.ReplaceAllString(s, "")
	return strings.TrimRight(s, "\r")
}

func (m *Manager) pipeConsole(in *Instance, rs *runtimeState, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := cleanLine(toUTF8(sc.Bytes()))
		if line == "" {
			continue
		}
		if m.tpsOnLine(rs, line) {
			continue // 面板静默 TPS 采样的响应，不进控制台
		}
		rs.console.Broadcast(line)

		m.healthOnLine(in, line)
		chatOnLine(in.ID, line)

		if nj := detectNeedJava(line, in.Version); nj > 0 {
			m.mu.Lock()
			rs.needJava = nj
			m.mu.Unlock()
		}

		if d := diagnose(line); d != "" {
			m.mu.Lock()
			if rs.diag == "" { // 只记第一个命中的诊断，避免刷屏
				rs.diag = d
				m.mu.Unlock()
				rs.console.Broadcast("[MCS 诊断] " + d)
			} else {
				m.mu.Unlock()
			}
		}

		if reDone.MatchString(line) {
			m.mu.Lock()
			rs.status = "running"
			rs.diag = "" // 启动成功，清掉误报
			m.mu.Unlock()
			m.addActivity("green", fmt.Sprintf("<b>%s</b> 启动完成，可以进入了", in.Name))
			m.notify("服务器启动完成", fmt.Sprintf("世界「%s」已启动完成（端口 %d），可以进入游戏了。", in.Name, in.Port))
			go m.frpAutoStart(in)
		} else if mt := reJoin.FindStringSubmatch(line); mt != nil {
			m.mu.Lock()
			rs.players[mt[1]] = true
			in.LastActive = time.Now()
			m.mu.Unlock()
			m.statsOnJoin(in.ID, mt[1])
			m.addActivity("green", fmt.Sprintf("<b>%s</b> 加入了 <b>%s</b>", mt[1], in.Name))
			if in.TalkInvite {
				go m.sendTalkInvite(in, mt[1])
			}
		} else if mt := reLeave.FindStringSubmatch(line); mt != nil {
			m.mu.Lock()
			delete(rs.players, mt[1])
			m.mu.Unlock()
			m.statsOnLeave(in.ID, mt[1])
			m.addActivity("blue", fmt.Sprintf("<b>%s</b> 离开了 <b>%s</b>", mt[1], in.Name))
		} else if mt := reAchv.FindStringSubmatch(line); mt != nil {
			m.addActivity("orange", fmt.Sprintf("<b>%s</b> 达成了成就「%s」", mt[1], mt[2]))
		}
	}
}

func (m *Manager) stopInstance(in *Instance) error {
	m.mu.Lock()
	rs := m.getRT(in.ID)
	if rs.status == "sleeping" {
		rs.status = "stopped"
		m.mu.Unlock()
		m.stopWakeListener(in.ID)
		rs.console.Broadcast("[MCS] 已退出待机")
		return nil
	}
	if rs.status != "running" && rs.status != "starting" {
		m.mu.Unlock()
		return fmt.Errorf("实例未在运行")
	}
	rs.status = "stopping"
	stdin := rs.stdin
	cmd := rs.cmd
	stopWord := "stop"
	if in.Type == "generic" {
		stopWord = in.StopCmd // 空 = 不发指令，超时后直接结束进程
	}
	m.mu.Unlock()

	if stdin != nil && stopWord != "" {
		io.WriteString(stdin, stopWord+"\r\n")
	}
	timeout := 30 * time.Second
	if in.Type == "generic" && stopWord == "" {
		timeout = 2 * time.Second // 没有停止指令的通用服务器，快速转为强制结束
	}
	go func() {
		time.Sleep(timeout)
		m.mu.Lock()
		still := rs.status == "stopping" && rs.cmd == cmd && cmd != nil
		m.mu.Unlock()
		if still && cmd.Process != nil {
			rs.console.Broadcast("[MCS] 停止超时，强制结束进程")
			killTree(cmd.Process.Pid)
		}
	}()
	return nil
}

func (m *Manager) sendCommand(in *Instance, command string) error {
	m.mu.Lock()
	rs := m.getRT(in.ID)
	stdin := rs.stdin
	ok := rs.status == "running" || rs.status == "starting"
	m.mu.Unlock()
	if !ok || stdin == nil {
		return fmt.Errorf("实例未在运行")
	}
	rs.console.Broadcast("> " + command)
	_, err := io.WriteString(stdin, command+"\r\n")
	return err
}

func findJava() string {
	if p, err := exec.LookPath("java"); err == nil {
		return p
	}
	var patterns []string
	if runtime.GOOS == "windows" {
		patterns = []string{
			`C:\Program Files\Java\*\bin\java.exe`,
			`C:\Program Files\Eclipse Adoptium\*\bin\java.exe`,
			`C:\Program Files\Microsoft\jdk*\bin\java.exe`,
			`C:\Program Files\Zulu\*\bin\java.exe`,
			`C:\Program Files (x86)\Java\*\bin\java.exe`,
		}
	} else {
		patterns = []string{
			"/usr/lib/jvm/*/bin/java",
			"/usr/local/openjdk*/bin/java",
			"/Library/Java/JavaVirtualMachines/*/Contents/Home/bin/java",
			"/opt/homebrew/opt/openjdk*/bin/java",
		}
	}
	for _, pat := range patterns {
		if ms, _ := filepath.Glob(pat); len(ms) > 0 {
			return ms[len(ms)-1]
		}
	}
	return ""
}
