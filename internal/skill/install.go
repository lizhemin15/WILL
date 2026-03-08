package skill

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var installClient = &http.Client{Timeout: 120 * time.Second}

// InstallFromURL 从 zip 或 tar.gz URL 下载并解压到 ~/.will/skills/<name>。name 为空则从归档内首目录名推断。
func InstallFromURL(url, name string) (installedDir string, err error) {
	home, _ := os.UserHomeDir()
	baseDir := filepath.Join(home, ".will", "skills")
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := installClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}

	lower := strings.ToLower(url)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(resp.Body, baseDir, name)
	case strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz"):
		return extractTarGz(resp.Body, baseDir, name)
	default:
		return "", fmt.Errorf("不支持的格式，请使用 .zip 或 .tar.gz 链接")
	}
}

func extractZip(r io.Reader, baseDir, name string) (string, error) {
	tmp, err := os.CreateTemp("", "will-skill-*.zip")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := io.Copy(tmp, r); err != nil {
		return "", err
	}
	_ = tmp.Sync()
	zr, err := zip.OpenReader(tmp.Name())
	if err != nil {
		return "", err
	}
	defer zr.Close()

	// 推断目标名：归档内第一个目录名，或传入的 name
	if name == "" {
		for _, f := range zr.File {
			if f.FileInfo().IsDir() && strings.Trim(f.Name, "/") != "" {
				parts := strings.Split(strings.Trim(f.Name, "/"), "/")
				name = parts[0]
				break
			}
		}
	}
	if name == "" {
		name = "skill"
	}
	dest := filepath.Join(baseDir, name)
	if err := os.MkdirAll(dest, 0755); err != nil {
		return "", err
	}
	// 若归档根目录是单一目录，解压时去掉该前缀，使 SKILL.md 直接在 dest 下
	var stripPrefix string
	for _, f := range zr.File {
		p := strings.Trim(f.Name, "/")
		if p == "" {
			continue
		}
		parts := strings.SplitN(p, "/", 2)
		if len(parts) == 2 && stripPrefix == "" {
			stripPrefix = parts[0] + "/"
		}
		break
	}
	for _, f := range zr.File {
		entryName := strings.Trim(f.Name, "/")
		if entryName == "" {
			continue
		}
		rel := entryName
		if stripPrefix != "" && strings.HasPrefix(entryName, stripPrefix) {
			rel = entryName[len(stripPrefix):]
		} else if strings.Contains(entryName, "/") {
			rel = entryName
		}
		fname := filepath.Join(dest, filepath.FromSlash(rel))
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(fname, 0755)
			continue
		}
		_ = os.MkdirAll(filepath.Dir(fname), 0755)
		out, err := os.Create(fname)
		if err != nil {
			return "", err
		}
		rc, _ := f.Open()
		_, _ = io.Copy(out, rc)
		rc.Close()
		out.Close()
	}
	return dest, nil
}

func extractTarGz(r io.Reader, baseDir, name string) (string, error) {
	tmp, err := os.CreateTemp("", "will-skill-*.tar.gz")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := io.Copy(tmp, r); err != nil {
		return "", err
	}
	_ = tmp.Sync()
	f, err := os.Open(tmp.Name())
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	tr := tar.NewReader(gz)
	if name == "" {
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			if h.Typeflag == tar.TypeDir && strings.Trim(h.Name, "/") != "" {
				parts := strings.Split(strings.Trim(h.Name, "/"), "/")
				name = parts[0]
				break
			}
		}
	}
	if name == "" {
		name = "skill"
	}
	gz.Close()
	if _, err := f.Seek(0, 0); err != nil {
		return "", err
	}
	gz, err = gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr = tar.NewReader(gz)
	dest := filepath.Join(baseDir, name)
	_ = os.MkdirAll(dest, 0755)
	var stripPrefix string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		entryName := strings.Trim(h.Name, "/")
		if entryName == "" {
			continue
		}
		if stripPrefix == "" && strings.Contains(entryName, "/") {
			stripPrefix = strings.SplitN(entryName, "/", 2)[0] + "/"
		}
		rel := entryName
		if stripPrefix != "" && strings.HasPrefix(entryName, stripPrefix) {
			rel = entryName[len(stripPrefix):]
		}
		fname := filepath.Join(dest, filepath.FromSlash(rel))
		switch h.Typeflag {
		case tar.TypeDir:
			_ = os.MkdirAll(fname, 0755)
		case tar.TypeReg:
			_ = os.MkdirAll(filepath.Dir(fname), 0755)
			out, _ := os.Create(fname)
			if out != nil {
				io.Copy(out, tr)
				out.Close()
			}
		}
	}
	gz.Close()
	return dest, nil
}
