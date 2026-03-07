package config

import (
	"os"
	"strings"

	"github.com/yourusername/will/internal/store"
)

type Mode string

const (
	ModeStandalone Mode = "standalone"
	ModeMain       Mode = "main"
	ModeWorker     Mode = "worker"
)

type Config struct {
	Mode Mode
	Bind string
	Port string

	LLMApiKey       string
	LLMBaseURL      string
	LLMModel        string

	FeishuAppID     string
	FeishuAppSecret string
	FeishuAllowed   []string

	InternalToken    string
	WorkerURLs       []string
	FeishuSubscribeMode string // "webhook" | "ws"，默认 ws（长连接）
}

// Load 仅从环境变量加载（worker 模式或未提供 store 时）
func Load() *Config {
	return loadFrom(nil)
}

// LoadFromStore 从 SQLite + 环境变量合并加载（env 优先）
func LoadFromStore(s *store.Store) *Config {
	return loadFrom(s)
}

func loadFrom(s *store.Store) *Config {
	get := func(envKey, configKey, defaultVal string) string {
		if v := os.Getenv(envKey); v != "" {
			return strings.TrimSpace(v)
		}
		if s != nil {
			if v, ok := s.GetConfig(configKey); ok && v != "" {
				return strings.TrimSpace(v)
			}
		}
		return defaultVal
	}

	port := get("PORT", store.ConfigKeyPort, "3000")
	bind := get("BIND", store.ConfigKeyBind, "0.0.0.0")
	modeStr := get("WILL_MODE", store.ConfigKeyMode, "standalone")
	mode := Mode(strings.ToLower(modeStr))
	if mode != ModeStandalone && mode != ModeMain && mode != ModeWorker {
		mode = ModeStandalone
	}

	var allowed []string
	if s != nil {
		allowed = s.GetAllowedOpenIDs()
	}
	if len(allowed) == 0 {
		if v := os.Getenv("FEISHU_ALLOWED_OPEN_IDS"); v != "" {
			for _, id := range strings.Split(v, ",") {
				id = strings.TrimSpace(id)
				if id != "" {
					allowed = append(allowed, id)
				}
			}
		}
	}

	var workerURLs []string
	if urls := get("WILL_WORKER_URLS", store.ConfigKeyWorkerURLs, ""); urls != "" {
		for _, u := range strings.Split(urls, ",") {
			u = strings.TrimSpace(u)
			if u != "" {
				workerURLs = append(workerURLs, u)
			}
		}
	}

	internalToken := get("WILL_INTERNAL_TOKEN", store.ConfigKeyInternalToken, "")
	subscribeMode := get("FEISHU_SUBSCRIBE_MODE", store.ConfigKeyFeishuSubscribeMode, "ws")
	if subscribeMode != "webhook" {
		subscribeMode = "ws"
	}

	return &Config{
		Mode:            mode,
		Bind:            bind,
		Port:            port,
		LLMApiKey:       get("OPENAI_API_KEY", store.ConfigKeyLLMApiKey, ""),
		LLMBaseURL:      get("OPENAI_BASE_URL", store.ConfigKeyLLMBaseURL, "https://api.openai.com/v1"),
		LLMModel:        get("OPENAI_MODEL", store.ConfigKeyLLMModel, "gpt-4o-mini"),
		FeishuAppID:     get("FEISHU_APP_ID", store.ConfigKeyFeishuAppID, ""),
		FeishuAppSecret: get("FEISHU_APP_SECRET", store.ConfigKeyFeishuAppSecret, ""),
		FeishuAllowed:   allowed,
		InternalToken:       internalToken,
		WorkerURLs:          workerURLs,
		FeishuSubscribeMode: subscribeMode,
	}
}
