package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ===== 单机存档导入 =====
// 把 .minecraft/saves 里的单人存档（zip）上传替换为服务器的 world/，
// 自动定位 level.dat 所在目录，无需玩家懂目录结构。

// findLevelDatDir returns the dir (relative walk) containing level.dat, searching
// up to a few levels deep; prefers the shallowest match.
func findLevelDatDir(root string) string {
	var best string
	bestDepth := 99
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.EqualFold(info.Name(), "level.dat") {
			dir := filepath.Dir(p)
			rel, _ := filepath.Rel(root, dir)
			depth := len(strings.Split(filepath.ToSlash(rel), "/"))
			if rel == "." {
				depth = 0
			}
			if depth < bestDepth {
				bestDepth = depth
				best = dir
			}
		}
		return nil
	})
	return best
}

func (m *Manager) handleWorldUpload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	if !ok {
		m.mu.Unlock()
		writeErr(w, 404, "实例不存在")
		return
	}
	rs := m.getRT(id)
	if rs.status != "stopped" && rs.status != "error" {
		m.mu.Unlock()
		writeErr(w, 409, "请先停止服务器再导入存档")
		return
	}
	rs.status = "downloading" // busy 锁，防导入期间启动
	rs.console.ClearProgress()
	name := in.Name
	m.mu.Unlock()

	release := func() {
		m.mu.Lock()
		rs.status = "stopped"
		m.mu.Unlock()
	}

	if err := r.ParseMultipartForm(2 << 30); err != nil { // 2GB
		release()
		writeErr(w, 400, "上传解析失败: "+err.Error())
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		release()
		writeErr(w, 400, "缺少文件")
		return
	}
	defer f.Close()
	if !strings.HasSuffix(strings.ToLower(hdr.Filename), ".zip") {
		release()
		writeErr(w, 400, "请上传 zip 压缩包（把存档文件夹压缩成 zip）")
		return
	}
	doBackup := r.FormValue("backup") == "true"

	dir := m.instDir(id)
	// 存到临时 zip
	tmpZip := filepath.Join(dir, ".world-upload.zip")
	out, err := os.Create(tmpZip)
	if err != nil {
		release()
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := io.Copy(out, f); err != nil {
		out.Close()
		os.Remove(tmpZip)
		release()
		writeErr(w, 500, err.Error())
		return
	}
	out.Close()
	defer os.Remove(tmpZip)

	// 解压到临时目录
	tmpDir := filepath.Join(dir, ".world-upload")
	os.RemoveAll(tmpDir)
	defer os.RemoveAll(tmpDir)
	if err := unzip(tmpZip, tmpDir); err != nil {
		release()
		writeErr(w, 500, "解压失败: "+err.Error())
		return
	}

	// 定位 level.dat
	src := findLevelDatDir(tmpDir)
	if src == "" {
		release()
		writeErr(w, 400, "压缩包里没找到 level.dat——请确认压缩的是存档文件夹（.minecraft/saves/<存档名>）")
		return
	}

	// 可选先备份
	if doBackup {
		if _, _, err := m.doBackup(id); err != nil {
			release()
			writeErr(w, 500, "导入前备份失败，已取消: "+err.Error())
			return
		}
	}

	// 替换世界（Paper 分维度目录一并清除，首次启动会从主世界数据重新生成/迁移）
	for _, d := range []string{"world", "world_nether", "world_the_end"} {
		os.RemoveAll(filepath.Join(dir, d))
	}
	if err := os.Rename(src, filepath.Join(dir, "world")); err != nil {
		// 跨目录 rename 同盘一般可行；失败则复制
		if cerr := copyTree(src, filepath.Join(dir, "world")); cerr != nil {
			release()
			writeErr(w, 500, "写入世界失败: "+cerr.Error())
			return
		}
	}
	// 单机存档的玩家数据在 level.dat 里绑的是单人 UUID，多人下会重新分配——正常现象
	release()
	m.addActivity("green", fmt.Sprintf("世界 <b>%s</b> 已导入单机存档「%s」", name, hdr.Filename))
	m.notify("单机存档导入完成", fmt.Sprintf("存档「%s」已导入到世界「%s」。启动服务器即可和朋友一起玩这个档。\n提示：单机的玩家背包/位置多人下会重置（存档地图、建筑完整保留）。", hdr.Filename, name))
	writeJSON(w, 200, map[string]bool{"ok": true})
}
