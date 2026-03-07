package updater

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const repo = "lizhemin15/WILL"
const apiLatest = "https://api.github.com/repos/" + repo + "/releases/latest"
const apiTagFmt = "https://api.github.com/repos/" + repo + "/releases/tags/v%s"

// Release 来自 GitHub API
type Release struct {
	TagName string  `json:"tag_name"`
	Body    string  `json:"body"`
	Assets  []Asset `json:"assets"`
}

type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// CheckLatest 获取最新发布版本及当前平台对应的 asset
func CheckLatest() (version string, assetURL string, err error) {
	req, _ := http.NewRequest(http.MethodGet, apiLatest, nil)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("github api: %d", resp.StatusCode)
	}
	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", "", err
	}
	version = strings.TrimPrefix(rel.TagName, "v")
	want := assetNameForPlatform()
	for _, a := range rel.Assets {
		if a.Name == want {
			return version, a.BrowserDownloadURL, nil
		}
	}
	return version, "", fmt.Errorf("未找到当前平台安装包 %s", want)
}

func assetNameForPlatform() string {
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "darwin"
	}
	arch := runtime.GOARCH
	return fmt.Sprintf("will-%s-%s.zip", osName, arch)
}

// DownloadAndApply 下载 zip，解压出可执行文件，生成更新脚本并执行后退出
func DownloadAndApply(assetURL string) error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	dir := filepath.Dir(exePath)
	zipPath := filepath.Join(dir, "will-update.zip")
	newExe := filepath.Join(dir, "will.new")
	if runtime.GOOS == "windows" {
		newExe = filepath.Join(dir, "will.new.exe")
	}

	// 下载
	resp, err := http.Get(assetURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(zipPath)
		return err
	}
	defer os.Remove(zipPath)

	// 解压 zip，找到 will 或 will.exe
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()
	var found bool
	for _, zf := range r.File {
		name := filepath.Base(zf.Name)
		if name == "will" || name == "will.exe" {
			rc, err := zf.Open()
			if err != nil {
				return err
			}
			out, err := os.Create(newExe)
			if err != nil {
				rc.Close()
				return err
			}
			_, err = io.Copy(out, rc)
			rc.Close()
			out.Close()
			if err != nil {
				os.Remove(newExe)
				return err
			}
			if err := os.Chmod(newExe, 0755); err != nil && runtime.GOOS != "windows" {
				os.Remove(newExe)
				return err
			}
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("zip 中未找到 will 可执行文件")
	}

	// 生成更新脚本：等待后替换并重启
	if runtime.GOOS == "windows" {
		bat := filepath.Join(dir, "will-updater.bat")
		content := fmt.Sprintf("@echo off\r\nping 127.0.0.1 -n 3 >nul\r\nmove /Y \"%s\" \"%s\"\r\nstart \"\" \"%s\"\r\ndel \"%%~f0\"\r\n", newExe, exePath, exePath)
		if err := os.WriteFile(bat, []byte(content), 0666); err != nil {
			return err
		}
		cmd := exec.Command("cmd", "/c", "start", "/b", bat)
		cmd.Dir = dir
		if err := cmd.Start(); err != nil {
			return err
		}
	} else {
		script := filepath.Join(dir, "will-updater.sh")
		content := fmt.Sprintf("#!/bin/sh\nsleep 2\nmv -f %q %q\nexec %q\n", newExe, exePath, exePath)
		if err := os.WriteFile(script, []byte(content), 0755); err != nil {
			return err
		}
		cmd := exec.Command("/bin/sh", script)
		cmd.Dir = dir
		cmd.Stdout = nil
		cmd.Stderr = nil
		if err := cmd.Start(); err != nil {
			return err
		}
	}
	os.Exit(0)
	return nil
}

// VersionCheckReply 根据 GitHub 发布检查版本，返回给用户看的文案（不执行 git）
func VersionCheckReply(currentVersion string) (reply string) {
	currentVersion = strings.TrimPrefix(currentVersion, "v")
	latestVer, _, err := CheckLatest()
	if err != nil {
		return "检查更新失败: " + err.Error()
	}
	if !CompareVersion(latestVer, currentVersion) {
		return "当前 v" + currentVersion + "，已是最新。"
	}
	return "当前 v" + currentVersion + "，发现新版本 v" + latestVer + "。回复「立即更新」可更新。"
}

// ReleaseNotes 拉取指定版本的 release 说明（body），失败返回空字符串
func ReleaseNotes(version string) string {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if version == "" {
		return ""
	}
	url := fmt.Sprintf(apiTagFmt, version)
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return ""
	}
	return strings.TrimSpace(rel.Body)
}

// CompareVersion 比较 a 与 b，若 a > b 返回 true
func CompareVersion(a, b string) bool {
	a = strings.TrimPrefix(a, "v")
	b = strings.TrimPrefix(b, "v")
	parse := func(s string) (major, minor, patch int) {
		parts := strings.Split(s, ".")
		if len(parts) > 0 {
			fmt.Sscanf(parts[0], "%d", &major)
		}
		if len(parts) > 1 {
			fmt.Sscanf(parts[1], "%d", &minor)
		}
		if len(parts) > 2 {
			fmt.Sscanf(parts[2], "%d", &patch)
		}
		return
	}
	ma, mia, pa := parse(a)
	mb, mib, pb := parse(b)
	if ma != mb {
		return ma > mb
	}
	if mia != mib {
		return mia > mib
	}
	return pa > pb
}

func init() {
	// 避免 GitHub API 限流
	http.DefaultClient.Timeout = 15 * time.Second
}
