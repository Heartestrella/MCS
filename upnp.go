package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/huin/goupnp/dcps/internetgateway2"
)

// ===== UPnP 端口映射（公网联机助手） =====

type igdClient interface {
	AddPortMapping(remoteHost string, externalPort uint16, protocol string, internalPort uint16, internalClient string, enabled bool, desc string, leaseDuration uint32) error
	DeletePortMapping(remoteHost string, externalPort uint16, protocol string) error
	GetExternalIPAddress() (string, error)
}

// findIGD returns the first WANIPConnection client found on the LAN.
func findIGD(ctx context.Context) (igdClient, error) {
	if cs, _, err := internetgateway2.NewWANIPConnection2ClientsCtx(ctx); err == nil && len(cs) > 0 {
		return cs[0], nil
	}
	if cs, _, err := internetgateway2.NewWANIPConnection1ClientsCtx(ctx); err == nil && len(cs) > 0 {
		return cs[0], nil
	}
	return nil, fmt.Errorf("局域网内没有发现支持 UPnP 的路由器（可能被路由器关闭了）")
}

const mappingLease = 0 // 永久（直到删除或路由器重启）

func (m *Manager) handleUpnpMap(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	var port int
	var name string
	if ok {
		port, name = in.Port, in.Name
	}
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	lan := lanIP()
	if lan == "" {
		writeErr(w, 500, "无法确定本机局域网 IP")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	igd, err := findIGD(ctx)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}

	if err := igd.AddPortMapping("", uint16(port), "TCP", uint16(port), lan, true, "MCS "+name, mappingLease); err != nil {
		writeErr(w, 502, "端口映射失败: "+err.Error())
		return
	}
	// 基岩互通装了就顺带映射 UDP 19132
	bedrock := false
	if g, f := m.geyserStatus(id); g && f {
		if err := igd.AddPortMapping("", 19132, "UDP", 19132, lan, true, "MCS Bedrock "+name, mappingLease); err == nil {
			bedrock = true
		}
	}

	ext, err := igd.GetExternalIPAddress()
	if err != nil {
		ext = ""
	}
	m.mu.Lock()
	in.UpnpMapped = true
	m.save()
	m.mu.Unlock()
	m.addActivity("green", fmt.Sprintf("<b>%s</b> 已开启公网映射（端口 %d）", name, port))
	writeJSON(w, 200, map[string]any{
		"ok":         true,
		"externalIP": ext,
		"port":       port,
		"bedrock":    bedrock,
	})
}

func (m *Manager) handleUpnpUnmap(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	var port int
	var name string
	if ok {
		port, name = in.Port, in.Name
	}
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	igd, err := findIGD(ctx)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	if err := igd.DeletePortMapping("", uint16(port), "TCP"); err != nil {
		writeErr(w, 502, "取消映射失败: "+err.Error())
		return
	}
	igd.DeletePortMapping("", 19132, "UDP") // 尽力而为
	m.mu.Lock()
	in.UpnpMapped = false
	m.save()
	m.mu.Unlock()
	m.addActivity("blue", fmt.Sprintf("<b>%s</b> 已关闭公网映射", name))
	writeJSON(w, 200, map[string]bool{"ok": true})
}
