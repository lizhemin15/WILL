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

	LLMApiKey  string
	LLMBaseURL string
	LLMModel   string

	FeishuAppID         string
	FeishuAppSecret     string
	FeishuAllowed       []string
	FeishuSubscribeMode string // "webhook" | "ws"

	InternalToken string
	WorkerURLs    []string
	MainURL       string // 从节点记录的主节点地址
	WorkerName    string // 从节点自定义名称

	Timezone string // IANA 时区，默认 Asia/Shanghai
}

func Load() *Config {
	return loadFrom(nil)
}

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
				if id = strings.TrimSpace(id); id != "" {
					allowed = append(allowed, id)
				}
			}
		}
	}

	var workerURLs []string
	if urls := get("WILL_WORKER_URLS", store.ConfigKeyWorkerURLs, ""); urls != "" {
		for _, u := range strings.Split(urls, ",") {
			if u = strings.TrimSpace(u); u != "" {
				workerURLs = append(workerURLs, u)
			}
		}
	}

	subscribeMode := get("FEISHU_SUBSCRIBE_MODE", store.ConfigKeyFeishuSubscribeMode, "ws")
	if subscribeMode != "webhook" {
		subscribeMode = "ws"
	}

	return &Config{
		Mode:                mode,
		Bind:                bind,
		Port:                port,
		LLMApiKey:           get("OPENAI_API_KEY", store.ConfigKeyLLMApiKey, ""),
		LLMBaseURL:          get("OPENAI_BASE_URL", store.ConfigKeyLLMBaseURL, "https://api.openai.com/v1"),
		LLMModel:            get("OPENAI_MODEL", store.ConfigKeyLLMModel, "gpt-4o-mini"),
		FeishuAppID:         get("FEISHU_APP_ID", store.ConfigKeyFeishuAppID, ""),
		FeishuAppSecret:     get("FEISHU_APP_SECRET", store.ConfigKeyFeishuAppSecret, ""),
		FeishuAllowed:       allowed,
		FeishuSubscribeMode: subscribeMode,
		InternalToken:       get("WILL_INTERNAL_TOKEN", store.ConfigKeyInternalToken, ""),
		WorkerURLs:          workerURLs,
		MainURL:             get("WILL_MAIN_URL", store.ConfigKeyMainURL, ""),
		WorkerName:          get("WILL_WORKER_NAME", store.ConfigKeyWorkerName, ""),
		Timezone:            get("WILL_TIMEZONE", store.ConfigKeyTimezone, "Asia/Shanghai"),
	}
}
