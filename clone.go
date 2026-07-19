package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ===== 一键克隆世界 =====
// 复刻一份完整实例（存档+模组+配置），自动换端口。
// 用途：开测试服试模组/新配置不动正式服、给朋友复制一份一样的世界。

// cloneSkip lists dir/file names not worth copying.
var cloneSkip = map[string]bool{
	"logs":          true,
	"cache":         true,
	"session.lock":  true,
	"crash-reports": true,
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // 被占用的文件跳过（如 latest.log）
		}
		rel, err := filepath.Rel(src, p)
		if err != nil || rel == "." {
			return nil
		}
		// 顶层跳过名单
		top := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
		if cloneSkip[top] {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		dest := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(dest, 0755)
		}
		in, err := os.Open(p)
		if err != nil {
			return nil // 锁定文件跳过
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return err
		}
		out, err := os.Create(dest)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		out.Close()
		return err
	})
}

// freePort finds an unused TCP port starting from base+1.
func (m *Manager) freePort(base int) int {
	used := map[int]bool{}
	m.mu.Lock()
	for _, in := range m.insts {
		used[in.Port] = true
	}
	m.mu.Unlock()
	for p := base + 1; p < base+200 && p < 65535; p++ {
		if used[p] {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err != nil {
			continue
		}
		ln.Close()
		return p
	}
	return base + 1
}

func (m *Manager) handleClone(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	src, ok := m.insts[id]
	if !ok {
		m.mu.Unlock()
		writeErr(w, 404, "实例不存在")
		return
	}
	running := m.getRT(id).status == "running"
	srcCopy := *src
	m.mu.Unlock()

	// 运行中先把世界刷到磁盘
	if running {
		m.sendCommand(&srcCopy, "save-all flush")
		time.Sleep(2 * time.Second)
	}

	newIn := srcCopy
	newIn.ID = newID()
	newIn.Dir = "" // 副本落在默认目录，不继承自定义路径
	newIn.Name = srcCopy.Name + " 副本"
	if len([]rune(newIn.Name)) > 30 {
		newIn.Name = string([]rune(newIn.Name)[:28]) + "副本"
	}
	newIn.Port = m.freePort(srcCopy.Port)
	newIn.CreatedAt = time.Now()
	newIn.LastActive = time.Now()
	newIn.AutoStart = false // 副本不继承自启，避免开机双开抢资源
	newIn.Status = "stopped"

	srcDir := m.instDir(id)
	dstDir := m.instDir(newIn.ID)
	if err := copyTree(srcDir, dstDir); err != nil {
		os.RemoveAll(dstDir)
		writeErr(w, 500, "复制文件失败: "+err.Error())
		return
	}

	// 改端口写回 server.properties
	propsPath := filepath.Join(dstDir, "server.properties")
	if props, err := readProps(propsPath); err == nil && props != nil {
		props["server-port"] = fmt.Sprintf("%d", newIn.Port)
		var sb strings.Builder
		sb.WriteString("# Cloned by MCS\n")
		for k, v := range props {
			sb.WriteString(k + "=" + v + "\n")
		}
		os.WriteFile(propsPath, []byte(sb.String()), 0644)
	}

	m.mu.Lock()
	m.insts[newIn.ID] = &newIn
	m.save()
	snap := m.snapshot(&newIn)
	m.mu.Unlock()

	m.addActivity("green", fmt.Sprintf("<b>%s</b> 克隆为 <b>%s</b>（端口 %d）", srcCopy.Name, newIn.Name, newIn.Port))
	m.notify("世界克隆完成", fmt.Sprintf("「%s」已克隆为「%s」，端口 %d，可独立启动。", srcCopy.Name, newIn.Name, newIn.Port))
	writeJSON(w, 200, snap)
}
