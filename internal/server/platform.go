package server

// 平台差异集中在这里:java 可执行名、shell、下载源命名、压缩格式。

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// javaBin is the JRE's executable name inside bin/.
func javaBin() string {
	if runtime.GOOS == "windows" {
		return "java.exe"
	}
	return "java"
}

// shellCommand runs a full command line through the platform shell.
func shellCommand(line string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/c", line)
	}
	return exec.Command("sh", "-c", line)
}

// runScriptName: Forge/NeoForge 官方安装器在两个平台各生成一个引导脚本。
func runScriptName() string {
	if runtime.GOOS == "windows" {
		return "run.bat"
	}
	return "run.sh"
}

// adoptiumOS / adoptiumArch map GOOS/GOARCH to Adoptium's path naming.
func adoptiumOS() string {
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "darwin":
		return "mac"
	default:
		return "linux"
	}
}

func adoptiumArch() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64"
	}
	return "x64"
}

// jreArchiveExt: Adoptium ships zip for windows, tar.gz elsewhere.
func jreArchiveExt() string {
	if runtime.GOOS == "windows" {
		return ".zip"
	}
	return ".tar.gz"
}

// findJavaIn returns a java executable under root, handling both the plain
// layout (jdk-21+x/bin) and the macOS bundle layout (Contents/Home/bin).
func findJavaIn(root string) string {
	if ms := findJavaAllIn(root); len(ms) > 0 {
		return ms[0]
	}
	return ""
}

func findJavaAllIn(root string) []string {
	var out []string
	for _, pat := range []string{
		filepath.Join(root, "*", "bin", javaBin()),
		filepath.Join(root, "*", "Contents", "Home", "bin", javaBin()),
	} {
		if ms, _ := filepath.Glob(pat); len(ms) > 0 {
			out = append(out, ms...)
		}
	}
	return out
}

// extractArchive unpacks zip or tar.gz (picked by filename) into dest.
func extractArchive(src, dest string) error {
	low := strings.ToLower(src)
	if strings.HasSuffix(low, ".tar.gz") || strings.HasSuffix(low, ".tgz") {
		return untarGz(src, dest)
	}
	return unzip(src, dest)
}

// untarGz extracts a .tar.gz preserving file modes (java 等需要可执行位),
// with the same path-traversal guard as unzip.
func untarGz(src, dest string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	destAbs, _ := filepath.Abs(dest)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		p := filepath.Join(dest, hdr.Name)
		pAbs, _ := filepath.Abs(p)
		if pAbs != destAbs && !strings.HasPrefix(pAbs, destAbs+string(os.PathSeparator)) {
			return fmt.Errorf("tar 路径非法: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(p, 0755)
		case tar.TypeSymlink:
			os.MkdirAll(filepath.Dir(p), 0755)
			os.Symlink(hdr.Linkname, p)
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
				return err
			}
			out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode&0777))
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			out.Close()
			if err != nil {
				return err
			}
		}
	}
}
