package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ===== 公开状态页 =====
// 服主开启后得到一个带随机 slug 的只读页面（免登录），
// 发给朋友即可随时看服务器在不在线、几人在玩、怎么连。

type statusPageCfg struct {
	Slug   string `json:"slug"` // 随机，防止被遍历
	InstID string `json:"instId"`
}

var statusPageMu sync.Mutex

func (m *Manager) statusPagesPath() string { return filepath.Join(m.dataDir, "statuspage.json") }

func (m *Manager) loadStatusPages() map[string]statusPageCfg { // key: instID
	out := map[string]statusPageCfg{}
	if b, err := os.ReadFile(m.statusPagesPath()); err == nil {
		json.Unmarshal(b, &out)
	}
	return out
}

func (m *Manager) saveStatusPages(v map[string]statusPageCfg) {
	b, _ := json.MarshalIndent(v, "", "  ")
	os.WriteFile(m.statusPagesPath(), b, 0644)
}

// handleStatusPageGet returns the share state for an instance (panel API).
func (m *Manager) handleStatusPageGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	_, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	statusPageMu.Lock()
	pages := m.loadStatusPages()
	cfg, on := pages[id]
	statusPageMu.Unlock()
	resp := map[string]any{"enabled": on}
	if on {
		resp["slug"] = cfg.Slug
	}
	writeJSON(w, 200, resp)
}

// handleStatusPageSet enables/disables the share page.
func (m *Manager) handleStatusPageSet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	statusPageMu.Lock()
	pages := m.loadStatusPages()
	if body.Enabled {
		if _, on := pages[id]; !on {
			pages[id] = statusPageCfg{Slug: randHex(8), InstID: id}
		}
	} else {
		delete(pages, id)
	}
	m.saveStatusPages(pages)
	cfg := pages[id]
	statusPageMu.Unlock()

	m.addActivity("blue", fmt.Sprintf("<b>%s</b> 公开状态页已%s", in.Name, map[bool]string{true: "开启", false: "关闭"}[body.Enabled]))
	resp := map[string]any{"enabled": body.Enabled}
	if body.Enabled {
		resp["slug"] = cfg.Slug
	}
	writeJSON(w, 200, resp)
}

// findBySlug returns the instance for a slug, or nil.
func (m *Manager) findBySlug(slug string) *Instance {
	if slug == "" {
		return nil
	}
	statusPageMu.Lock()
	pages := m.loadStatusPages()
	statusPageMu.Unlock()
	for _, cfg := range pages {
		if cfg.Slug == slug {
			m.mu.Lock()
			in := m.insts[cfg.InstID]
			m.mu.Unlock()
			return in
		}
	}
	return nil
}

// handlePublicStatus is the JSON feed for the public page (no auth).
func (m *Manager) handlePublicStatus(w http.ResponseWriter, r *http.Request) {
	in := m.findBySlug(r.PathValue("slug"))
	if in == nil {
		writeErr(w, 404, "页面不存在或已关闭")
		return
	}
	m.mu.Lock()
	snap := m.snapshot(in)
	m.mu.Unlock()

	props, _ := readProps(m.propsPath(in.ID))
	motd := ""
	maxPlayers := ""
	if props != nil {
		motd = props["motd"]
		maxPlayers = props["max-players"]
	}

	// 最近 24h 在线曲线（借用玩家统计）
	statsMu.Lock()
	st := m.getStats(in.ID)
	cut := time.Now().Add(-24 * time.Hour)
	var samples []statSample
	for _, s := range st.Samples {
		if s.T.After(cut) {
			samples = append(samples, s)
		}
	}
	statsMu.Unlock()

	// 语音房:在线人数 + 免登录入口(frp 穿透优先,其次局域网 HTTPS)
	talkCount := 0
	talkURL := ""
	if talkSrv != nil {
		room := talkRoomOf(in.ID)
		talkSrv.mu.Lock()
		if tr := talkSrv.rooms[room]; tr != nil {
			talkCount = len(tr.peers)
		}
		talkSrv.mu.Unlock()
		talkURL, _ = m.talkURLFor(in.ID)
	}

	writeJSON(w, 200, map[string]any{
		"name":       in.Name,
		"status":     snap.Status,
		"players":    snap.Players,
		"playerList": snap.PlayerList,
		"maxPlayers": maxPlayers,
		"version":    in.Version,
		"type":       in.Type,
		"motd":       motd,
		"addr":       fmt.Sprintf("%s:%d", lanIP(), in.Port),
		"uptimeSec":  snap.UptimeSec,
		"samples":    samples,
		"talkUrl":    talkURL,
		"talkCount":  talkCount,
	})
}

// handlePublicPage serves the standalone HTML page (no auth).
func (m *Manager) handlePublicPage(w http.ResponseWriter, r *http.Request) {
	in := m.findBySlug(r.PathValue("slug"))
	if in == nil {
		http.Error(w, "页面不存在或已关闭", 404)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, publicPageHTML, r.PathValue("slug"))
}

const publicPageHTML = `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>服务器状态</title>
<style>
  body{margin:0;font-family:system-ui,'Microsoft YaHei',sans-serif;background:#0c0a1d;color:#c8c4e0;
       display:flex;justify-content:center;align-items:flex-start;min-height:100vh;padding:40px 16px}
  .card{width:100%%;max-width:480px;background:rgba(255,255,255,.04);border:1px solid rgba(255,255,255,.09);
        border-radius:20px;padding:28px}
  h1{font-size:22px;margin:0 0 4px;color:#fff}
  .motd{color:#9a96b8;font-size:13px;margin-bottom:18px;white-space:pre-wrap}
  .pill{display:inline-block;padding:4px 14px;border-radius:99px;font-size:13px;font-weight:700}
  .on{background:rgba(106,255,176,.12);color:#6affb0}
  .off{background:rgba(255,255,255,.08);color:#9a96b8}
  .slp{background:rgba(255,179,92,.12);color:#ffb35c}
  .row{display:flex;justify-content:space-between;padding:9px 0;border-bottom:1px solid rgba(255,255,255,.06);font-size:14px}
  .row b{color:#fff}
  .addr{cursor:pointer;color:#7aa2ff}
  canvas{width:100%%;background:rgba(255,255,255,.03);border-radius:12px;margin-top:16px}
  .foot{margin-top:16px;font-size:11px;color:#6e6a8e;text-align:center}
  .players{font-size:12px;color:#9a96b8;margin-top:6px;line-height:1.7}
  .talkbtn{display:none;margin-top:16px;width:100%%;padding:12px;border:none;border-radius:12px;
           background:rgba(199,146,255,.15);color:#c792ff;font-size:14px;font-weight:700;cursor:pointer}
  .talkbtn:hover{background:rgba(199,146,255,.25)}
</style>
</head>
<body>
<div class="card">
  <h1 id="name">加载中…</h1>
  <div class="motd" id="motd"></div>
  <div><span class="pill off" id="pill">…</span></div>
  <div style="margin-top:14px">
    <div class="row"><span>在线玩家</span><b id="players">-</b></div>
    <div class="row"><span>版本</span><b id="version">-</b></div>
    <div class="row"><span>连接地址（点击复制）</span><b class="addr" id="addr" onclick="copyAddr()">-</b></div>
    <div class="row" id="uptimeRow" style="display:none"><span>已运行</span><b id="uptime">-</b></div>
  </div>
  <div class="players" id="playerList"></div>
  <button class="talkbtn" id="talkBtn" onclick="window.open(talkUrl,'_blank')">进入语音房</button>
  <canvas id="chart" height="110"></canvas>
  <div class="foot">近 24 小时在线人数 · 每 10 秒自动刷新 · Powered by MCS</div>
</div>
<script>
const slug = %q;
let addr = '';
let talkUrl = '';
function copyAddr(){ navigator.clipboard && navigator.clipboard.writeText(addr).then(()=>{ document.getElementById('addr').textContent='已复制!'; setTimeout(load, 800); }); }
function fmtDur(s){ if(s<3600) return Math.floor(s/60)+' 分钟'; return Math.floor(s/3600)+' 小时 '+Math.floor(s%%3600/60)+' 分'; }
async function load(){
  try{
    const d = await (await fetch('/api/public/status/'+slug)).json();
    if(d.error){ document.getElementById('name').textContent = d.error; return; }
    document.getElementById('name').textContent = d.name;
    document.getElementById('motd').textContent = (d.motd||'').replace(/§./g,'');
    const pill = document.getElementById('pill');
    const st = d.status;
    if(st==='running'){ pill.className='pill on'; pill.textContent='在线，可以进入'; }
    else if(st==='sleeping'){ pill.className='pill slp'; pill.textContent='待机中 · 进入游戏自动唤醒'; }
    else if(st==='starting'){ pill.className='pill slp'; pill.textContent='正在启动…'; }
    else { pill.className='pill off'; pill.textContent='离线'; }
    document.getElementById('players').textContent = d.players + (d.maxPlayers? ' / '+d.maxPlayers : '');
    document.getElementById('version').textContent = (d.type||'') + ' ' + (d.version||'');
    addr = d.addr; document.getElementById('addr').textContent = d.addr;
    const ur = document.getElementById('uptimeRow');
    if(d.uptimeSec){ ur.style.display=''; document.getElementById('uptime').textContent = fmtDur(d.uptimeSec); } else ur.style.display='none';
    document.getElementById('playerList').textContent = (d.playerList&&d.playerList.length) ? '在玩：'+d.playerList.join('、') : '';
    if(d.talkUrl){
      talkUrl = d.talkUrl;
      const tb = document.getElementById('talkBtn');
      tb.style.display = 'block';
      tb.textContent = d.talkCount > 0 ? '进入语音房（'+d.talkCount+' 人在聊）' : '进入语音房 · 和大家开黑聊天';
    }
    drawChart(d.samples||[]);
  }catch(e){}
}
function drawChart(samples){
  const cv = document.getElementById('chart');
  const W = cv.clientWidth, H = 110;
  cv.width = W*devicePixelRatio; cv.height = H*devicePixelRatio;
  const ctx = cv.getContext('2d'); ctx.scale(devicePixelRatio, devicePixelRatio);
  ctx.clearRect(0,0,W,H);
  const now = Date.now(), from = now - 24*3600e3;
  const pts = samples.map(s=>({t:new Date(s.t).getTime(), n:s.n})).filter(p=>p.t>=from);
  if(!pts.length){ ctx.fillStyle='#6e6a8e'; ctx.font='12px sans-serif'; ctx.textAlign='center'; ctx.fillText('暂无数据', W/2, H/2); return; }
  const maxN = Math.max(2, ...pts.map(p=>p.n));
  const px = t => 6+(t-from)/(now-from)*(W-12), py = n => H-12-n/maxN*(H-24);
  ctx.beginPath(); pts.forEach((p,i)=> i?ctx.lineTo(px(p.t),py(p.n)):ctx.moveTo(px(p.t),py(p.n)));
  ctx.strokeStyle='#7aa2ff'; ctx.lineWidth=1.5; ctx.stroke();
  ctx.lineTo(px(pts[pts.length-1].t),H-12); ctx.lineTo(px(pts[0].t),H-12); ctx.closePath();
  ctx.fillStyle='rgba(122,162,255,.12)'; ctx.fill();
}
load(); setInterval(load, 10000);
</script>
</body>
</html>`
