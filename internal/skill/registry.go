package skill

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

const (
	registryEnv   = "WILL_SKILLS_REGISTRY_URL"
	defaultRegURL = "" // 未设置时需显式配置；可指向 https://raw.githubusercontent.com/xxx/WILL/main/registry/skills.json
)

var registryClient = &http.Client{Timeout: 15 * time.Second}

// RegistryEntry 注册表中一条 Skill 信息
type RegistryEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	URL         string `json:"url"`     // 安装包 zip/tar.gz 地址
	Homepage    string `json:"homepage,omitempty"`
	Version     string `json:"version,omitempty"`
}

// FetchRegistry 从配置的 URL 拉取注册表；需设置 WILL_SKILLS_REGISTRY_URL。
func FetchRegistry() ([]RegistryEntry, error) {
	url := os.Getenv(registryEnv)
	if url == "" {
		url = defaultRegURL
	}
	if url == "" {
		return nil, fmt.Errorf("未设置 WILL_SKILLS_REGISTRY_URL，无法拉取注册表")
	}
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

type registryError struct{ status int }

func (e *registryError) Error() string { return fmt.Sprintf("registry: HTTP %d", e.status) }
