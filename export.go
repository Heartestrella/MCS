package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ===== 客户端整合包导出 =====
// 把模组服的 mods/config 打包成 .mrpack（Modrinth App / Prism / HMCL 可直接导入）
// 或普通 zip（手动装），让朋友一键配好和服务器一致的客户端。

// detectLoaderVersion best-effort finds the loader version from instance files.
func detectLoaderVersion(dir, typ string) string {
	switch typ {
	case "fabric":
		// fabric-server 会把 loader 放进 libraries/net/fabricmc/fabric-loader/<ver>/
		if ms, _ := filepath.Glob(filepath.Join(dir, "libraries", "net", "fabricmc", "fabric-loader", "*")); len(ms) > 0 {
			return filepath.Base(ms[len(ms)-1])
		}
		// 或 .fabric/remappedJars/minecraft-<mc>-<loader> 之类，放弃时返回空
	case "forge":
		if ms, _ := filepath.Glob(filepath.Join(dir, "libraries", "net", "minecraftforge", "forge", "*")); len(ms) > 0 {
			// 目录名形如 1.20.1-47.2.20
			base := filepath.Base(ms[len(ms)-1])
			if i := strings.Index(base, "-"); i > 0 {
				return base[i+1:]
			}
			return base
		}
	case "neoforge":
		if ms, _ := filepath.Glob(filepath.Join(dir, "libraries", "net", "neoforged", "neoforge", "*")); len(ms) > 0 {
			return filepath.Base(ms[len(ms)-1])
		}
	}
	return ""
}

var unsafeFileRe = regexp.MustCompile(`[<>:"/\\|?*]`)

// addFileToZip writes a disk file into the zip under zipPath.
func addFileToZip(zw *zip.Writer, diskPath, zipPath string) error {
	f, err := os.Open(diskPath)
	if err != nil {
		return err
	}
	defer f.Close()
	w, err := zw.Create(zipPath)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}

// collectClientFiles returns enabled mod jars and config files for export.
func collectClientFiles(dir string) (mods []string, configs []string) {
	if entries, err := os.ReadDir(filepath.Join(dir, "mods")); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".jar") {
				continue
			}
			mods = append(mods, filepath.Join(dir, "mods", e.Name()))
		}
	}
	// config 目录整体带上（很多模组需要一致配置）；跳过超过 2MB 的杂物
	cfgRoot := filepath.Join(dir, "config")
	filepath.Walk(cfgRoot, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Size() > 2*1024*1024 {
			return nil
		}
		configs = append(configs, p)
		return nil
	})
	return
}

func (m *Manager) handleExport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	in, ok := m.insts[id]
	m.mu.Unlock()
	if !ok {
		writeErr(w, 404, "实例不存在")
		return
	}
	if in.Type != "fabric" && in.Type != "forge" && in.Type != "neoforge" {
		writeErr(w, 400, "只有模组服（Fabric/Forge/NeoForge）需要导出客户端包；Paper 插件服朋友直接用原版客户端进入即可")
		return
	}
	dir := m.instDir(id)
	mods, configs := collectClientFiles(dir)
	if len(mods) == 0 {
		writeErr(w, 400, "mods 目录里没有可导出的模组")
		return
	}

	format := r.URL.Query().Get("format")
	if format == "" {
		format = "mrpack"
	}
	safeName := unsafeFileRe.ReplaceAllString(in.Name, "_")
	loaderVer := detectLoaderVersion(dir, in.Type)

	if format == "mrpack" {
		w.Header().Set("Content-Type", "application/x-modrinth-modpack+zip")
		w.Header().Set("Content-Disposition", `attachment; filename*=UTF-8''`+url.PathEscape(safeName+"-客户端.mrpack"))
		zw := zip.NewWriter(w)
		defer zw.Close()

		deps := map[string]string{"minecraft": in.Version}
		if loaderVer != "" {
			switch in.Type {
			case "fabric":
				deps["fabric-loader"] = loaderVer
			case "forge":
				deps["forge"] = loaderVer
			case "neoforge":
				deps["neoforge"] = loaderVer
			}
		}
		index := map[string]any{
			"formatVersion": 1,
			"game":          "minecraft",
			"versionId":     "1.0.0",
			"name":          in.Name + " 客户端",
			"summary":       "由 MCS 面板从服务器导出，导入后模组和配置与服务器一致",
			"files":         []any{},
			"dependencies":  deps,
		}
		iw, _ := zw.Create("modrinth.index.json")
		json.NewEncoder(iw).Encode(index)

		for _, p := range mods {
			addFileToZip(zw, p, "overrides/mods/"+filepath.Base(p))
		}
		for _, p := range configs {
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				continue
			}
			addFileToZip(zw, p, "overrides/"+filepath.ToSlash(rel))
		}
	} else {
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename*=UTF-8''`+url.PathEscape(safeName+"-客户端模组.zip"))
		zw := zip.NewWriter(w)
		defer zw.Close()

		loaderName := map[string]string{"fabric": "Fabric", "forge": "Forge", "neoforge": "NeoForge"}[in.Type]
		readme := fmt.Sprintf("这是「%s」服务器的客户端模组包\r\n\r\n使用方法：\r\n1. 安装 Minecraft %s 和 %s%s\r\n2. 把 mods 文件夹里的所有文件复制到你的 .minecraft/mods/\r\n3. 把 config 文件夹（如有）复制到 .minecraft/config/\r\n4. 启动游戏，服务器地址问服主要\r\n\r\n提示：更推荐用 Modrinth App / Prism / HMCL 等启动器直接导入 .mrpack 版本，全自动。\r\n",
			in.Name, in.Version, loaderName,
			map[bool]string{true: "（版本 " + loaderVer + "）", false: ""}[loaderVer != ""])
		rw, _ := zw.Create("使用说明.txt")
		rw.Write([]byte("\xEF\xBB\xBF" + readme)) // BOM 让记事本正确显示 UTF-8

		for _, p := range mods {
			addFileToZip(zw, p, "mods/"+filepath.Base(p))
		}
		for _, p := range configs {
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				continue
			}
			addFileToZip(zw, p, filepath.ToSlash(rel))
		}
	}
	m.addActivity("green", fmt.Sprintf("<b>%s</b> 导出了客户端整合包（%s）", in.Name, format))
}
