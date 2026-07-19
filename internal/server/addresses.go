package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ===== 联机地址中心 =====
// 汇总一个实例所有可用的连接地址（局域网 / UPnP 公网 / frp 穿透），
// 标注适用场景与推荐项，回答服主最常见的问题：「我该把哪个地址发给朋友？」

type connAddr struct {
	Kind      string `json:"kind"`  // lan / upnp / frp
	Addr      string `json:"addr"`  // host:port
	Label     string `json:"label"` // 适用说明
	Warn      string `json:"warn,omitempty"`
	Recommend bool   `json:"recommend"`
}

// isPrivateIP reports whether ip is RFC1918 / CGNAT (100.64/10) / link-local.
func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return true
	}
	// CGNAT 100.64.0.0/10
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return true
	}
	return false
}

// publicIPCache avoids hammering IP echo services.
var publicIPCache struct {
	sync.Mutex
	ip string
	at time.Time
}

// publicIP fetches the machine's real public IPv4, bypassing HTTP proxies
// (proxies would report the proxy exit IP, useless for hosting).
func publicIP() string {
	publicIPCache.Lock()
	if time.Since(publicIPCache.at) < 5*time.Minute && publicIPCache.ip != "" {
		ip := publicIPCache.ip
		publicIPCache.Unlock()
		return ip
	}
	publicIPCache.Unlock()

	client := &http.Client{
		Timeout:   6 * time.Second,
		Transport: &http.Transport{Proxy: nil}, // 绕过环境变量代理
	}
	for _, u := range []string{"https://ip.3322.net", "https://ifconfig.me/ip", "https://api.ipify.org"} {
		resp, err := client.Get(u)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		ip := strings.TrimSpace(string(b))
		if net.ParseIP(ip) == nil {
			continue
		}
		publicIPCache.Lock()
		publicIPCache.ip, publicIPCache.at = ip, time.Now()
		publicIPCache.Unlock()
		return ip
	}
	return ""
}

func (m *Manager) handleAddresses(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	var port int
	var upnpOn bool
	if ok {
		port = in.Port
		upnpOn = in.UpnpMapped
	}
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}

	var out []connAddr

	// 1. 局域网
	if lan := lanIP(); lan != "" {
		out = append(out, connAddr{
			Kind: "lan", Addr: fmt.Sprintf("%s:%d", lan, port),
			Label: "同一 WiFi / 路由器下的朋友用这个",
		})
	}

	// 2. frp 穿透（连通时优先推荐）
	frpMu.Lock()
	frs := frpGetRT(id)
	frpAddr, frpStatus := frs.addr, frs.status
	frpMu.Unlock()
	if frpStatus == "running" && frpAddr != "" {
		out = append(out, connAddr{
			Kind: "frp", Addr: frpAddr,
			Label:     "任何地方的朋友都能连（frp 穿透）",
			Recommend: true,
		})
	}

	// 3. UPnP 公网（已开映射才展示；检测 CGNAT）
	upnpShown := false
	if upnpOn {
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		igd, err := findIGD(ctx)
		cancel()
		if err == nil {
			ext, _ := igd.GetExternalIPAddress()
			if ext != "" {
				upnpShown = true
				a := connAddr{
					Kind: "upnp", Addr: fmt.Sprintf("%s:%d", ext, port),
					Label: "异地朋友直连（UPnP 端口映射）",
				}
				if isPrivateIP(net.ParseIP(ext)) {
					a.Warn = "路由器拿到的不是真公网 IP（运营商 CGNAT），这个地址朋友连不上，请改用 frp 穿透"
				} else if real := publicIP(); real != "" && real != ext {
					a.Warn = fmt.Sprintf("路由器上层还有一层 NAT（真实出口 %s），大概率连不通，建议用 frp", real)
				} else if frpStatus != "running" {
					a.Recommend = true
				}
				out = append(out, a)
			}
		}
	}

	// 4. 真实公网 IP（没开 UPnP 时给手动端口转发的用户参考）
	if !upnpShown {
		if real := publicIP(); real != "" {
			out = append(out, connAddr{
				Kind: "public", Addr: fmt.Sprintf("%s:%d", real, port),
				Label: "本机公网出口 IP —— 仅当你已在路由器手动做了端口转发才可用",
				Warn:  "没做过端口转发的话此地址连不通；异地联机推荐用下方 UPnP 或 frp",
			})
		}
	}

	writeJSON(w, 200, map[string]any{"addresses": out, "upnpMapped": upnpOn})
}

// ===== 公网 IP 变化监控 =====
// 家宽公网 IP 会不定期变化，朋友手里的旧地址就失效了。
// 只要有实例开着 UPnP 映射，每 10 分钟对比一次公网 IP，变了就通知。

func (m *Manager) startIPWatcher() {
	go func() {
		var lastIP string
		for range time.Tick(10 * time.Minute) {
			m.mu.Lock()
			var mapped []*Instance
			for _, in := range m.insts {
				if in.UpnpMapped {
					mapped = append(mapped, in)
				}
			}
			m.mu.Unlock()
			if len(mapped) == 0 {
				lastIP = ""
				continue
			}
			ip := publicIP()
			if ip == "" {
				continue
			}
			if lastIP == "" {
				lastIP = ip
				continue
			}
			if ip == lastIP {
				continue
			}
			old := lastIP
			lastIP = ip
			var lines []string
			for _, in := range mapped {
				lines = append(lines, fmt.Sprintf("%s → %s:%d", in.Name, ip, in.Port))
			}
			m.addActivity("orange", fmt.Sprintf("公网 IP 已变化 %s → <b>%s</b>，记得把新地址发给朋友", old, ip))
			m.notify("公网 IP 已变化，朋友需要新地址",
				fmt.Sprintf("检测到本机公网 IP 从 %s 变为 %s。\n开启了公网映射的世界新地址：\n%s\n\n（IP 变化是运营商行为，属正常现象；不想每次换地址可以在联机页改用 frp 穿透）",
					old, ip, strings.Join(lines, "\n")))
		}
	}()
}
