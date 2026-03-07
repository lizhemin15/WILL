package updater

import (
	"archive/zip"
	"bytes"
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

// AssetNameForPlatform 返回指定 OS/Arch 的发布包文件名
func AssetNameForPlatform(osName, arch string) string {
	return fmt.Sprintf("will-%s-%s.zip", osName, arch)
}

// CheckLatestForPlatform 获取最新版本中指定平台对应的 asset URL
func CheckLatestForPlatform(osName, arch string) (version string, assetURL string, err error) {
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
	want := AssetNameForPlatform(osName, arch)
	for _, a := range rel.Assets {
		if a.Name == want {
			return version, a.BrowserDownloadURL, nil
		}
	}
	return version, "", fmt.Errorf("未找到平台 %s/%s 的安装包 %s", osName, arch, want)
}

// CheckLatest 获取最新发布版本及当前平台对应的 asset
func CheckLatest() (version string, assetURL string, err error) {
	return CheckLatestForPlatform(runtime.GOOS, runtime.GOARCH)
}

// DownloadZip 下载 assetURL 并返回 zip 原始字节
func DownloadZip(assetURL string) ([]byte, error) {
	resp, err := http.Get(assetURL) //nolint:noctx
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// ApplyFromBytes 从 zip 字节流中提取 will 可执行文件，生成更新脚本后退出进程完成热替换。
// 用于从节点接收主节点推送的二进制包时直接应用，无需自行访问 GitHub。
func ApplyFromBytes(zipData []byte) error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	dir := filepath.Dir(exePath)
	newExe := filepath.Join(dir, "will.new")
	if runtime.GOOS == "windows" {
		newExe = filepath.Join(dir, "will.new.exe")
	}

	// 内存解压 zip
	r, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return err
	}
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

	// 生成更新脚本：等待进程退出后替换并重启
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

// DownloadAndApply 下载 zip 并应用更新（主节点自升级时使用）
func DownloadAndApply(assetURL string) error {
	data, err := DownloadZip(assetURL)
	if err != nil {
		return err
	}
	return ApplyFromBytes(data)
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
