package skill

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	registryEnv   = "WILL_SKILLS_REGISTRY_URL"
	defaultRegURL = ""

	// OpenClaw 生态：默认使用 GitHub 上的聚合仓库（与 ClawHub 兼容的 SKILL.md 格式），无需配置 URL
	githubSkillsRepo = "LeoYeAI/openclaw-master-skills"
	githubSkillsPath = "skills"
	githubAPIList   = "https://api.github.com/repos/" + githubSkillsRepo + "/contents/" + githubSkillsPath
	githubZipURL    = "https://github.com/" + githubSkillsRepo + "/archive/refs/heads/main.zip"
)

var registryClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{},
}

// RegistryEntry 注册表中一条 Skill 信息
type RegistryEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	URL         string `json:"url"`     // 安装包 zip/tar.gz 地址，或 GitHub 整库 zip
	Homepage    string `json:"homepage,omitempty"`
	Version     string `json:"version,omitempty"`
	RepoSubpath string `json:"-"` // 从整库 zip 中只解压的子路径，如 skills/pdf
}

// FetchRegistry 拉取可安装的 Skill 列表。未配置 WILL_SKILLS_REGISTRY_URL 时默认使用 OpenClaw 生态仓库（GitHub）。
func FetchRegistry() ([]RegistryEntry, error) {
	url := strings.TrimSpace(os.Getenv(registryEnv))
	if url != "" {
		return fetchRegistryFromJSON(url)
	}
	return fetchRegistryFromGitHub()
}

func fetchRegistryFromJSON(url string) ([]RegistryEntry, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := registryClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &registryError{status: resp.StatusCode}
	}
	var list []RegistryEntry
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	return list, nil
}

// fetchRegistryFromGitHub 从 OpenClaw 生态 GitHub 仓库列出 skills 目录下所有子目录，无需配置。
func fetchRegistryFromGitHub() ([]RegistryEntry, error) {
	req, err := http.NewRequest(http.MethodGet, githubAPIList, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "WILL/1.0")
	resp, err := registryClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &registryError{status: resp.StatusCode}
	}
	var contents []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&contents); err != nil {
		return nil, err
	}
	var list []RegistryEntry
	for _, c := range contents {
		if c.Type != "dir" || c.Name == "" || strings.HasPrefix(c.Name, ".") {
			continue
		}
		list = append(list, RegistryEntry{
			Name:        c.Name,
			Description: "OpenClaw/ClawHub 兼容 Skill（来自 " + githubSkillsRepo + "）",
			URL:         githubZipURL,
			Homepage:    "https://github.com/" + githubSkillsRepo + "/tree/main/" + githubSkillsPath + "/" + c.Name,
			RepoSubpath: githubSkillsPath + "/" + c.Name,
		})
	}
	return list, nil
}

type registryError struct{ status int }

func (e *registryError) Error() string { return fmt.Sprintf("registry: HTTP %d", e.status) }
