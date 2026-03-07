package setup

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/yourusername/will/internal/config"
	"github.com/yourusername/will/internal/feishu"
	"github.com/yourusername/will/internal/llm"
	"github.com/yourusername/will/internal/store"
)

const urlHint = "只填到域名或 /v1，不要加 /chat/completions 等路径。例如: https://api.openai.com/v1"

// RunStartup 启动时检测 LLM、飞书配置，缺失或校验失败则交互填写，直到通过或用户跳过（飞书可跳过）
func RunStartup(s *store.Store) *config.Config {
	if s == nil {
		return nil
	}
	_ = ensureLLM(s)
	ensureFeishuOrSkip(s)
	return config.LoadFromStore(s)
}

func ensureLLM(s *store.Store) *config.Config {
	reader := bufio.NewReader(os.Stdin)
	for {
		cfg := config.LoadFromStore(s)
		if cfg.LLMApiKey == "" && os.Getenv("OPENAI_API_KEY") == "" {
			fmt.Fprintln(os.Stdout, "WILL 未检测到 LLM 配置，请按提示输入（必填）。")
			fmt.Fprintln(os.Stdout, "")
			promptLLMOnce(s, reader)
			if err := llm.TestConfig(cfgFromStore(s)); err != nil {
				fmt.Fprintln(os.Stdout, "LLM 校验失败:", err)
				continue
			}
			fmt.Fprintln(os.Stdout, "LLM 校验通过。")
			break
		}
		if err := llm.TestConfig(cfg); err != nil {
			fmt.Fprintln(os.Stdout, "LLM 校验失败:", err)
			fmt.Fprintln(os.Stdout, "请重新输入正确的 LLM 配置。")
			promptLLMOnce(s, reader)
			if err := llm.TestConfig(cfgFromStore(s)); err != nil {
				fmt.Fprintln(os.Stdout, "LLM 校验仍失败:", err)
				continue
			}
		}
		fmt.Fprintln(os.Stdout, "LLM 校验通过。")
		break
	}
	return config.LoadFromStore(s)
}

func cfgFromStore(s *store.Store) *config.Config {
	key, _ := s.GetConfig(store.ConfigKeyLLMApiKey)
	baseURL, _ := s.GetConfig(store.ConfigKeyLLMBaseURL)
	model, _ := s.GetConfig(store.ConfigKeyLLMModel)
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	if model == "" {
		model = "gpt-4o-mini"
	}
	return &config.Config{LLMApiKey: key, LLMBaseURL: baseURL, LLMModel: model}
}

func promptLLMOnce(s *store.Store, reader *bufio.Reader) {
	fmt.Fprint(os.Stdout, "LLM API Key (必填): ")
	key, _ := reader.ReadString('\n')
	key = strings.TrimSpace(key)
	if key == "" {
		fmt.Fprintln(os.Stdout, "未输入 API Key，退出。")
		os.Exit(1)
	}
	fmt.Fprintf(os.Stdout, "LLM Base URL (%s): ", urlHint)
	baseURL, _ := reader.ReadString('\n')
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	fmt.Fprint(os.Stdout, "LLM Model (回车默认 gpt-4o-mini): ")
	model, _ := reader.ReadString('\n')
	model = strings.TrimSpace(model)
	if model == "" {
		model = "gpt-4o-mini"
	}
	_ = s.SetConfig(store.ConfigKeyLLMApiKey, key)
	_ = s.SetConfig(store.ConfigKeyLLMBaseURL, baseURL)
	_ = s.SetConfig(store.ConfigKeyLLMModel, model)
	fmt.Fprintln(os.Stdout, "已保存，正在校验…")
}

func ensureFeishuOrSkip(s *store.Store) {
	reader := bufio.NewReader(os.Stdin)
	for {
		cfg := config.LoadFromStore(s)
		if cfg.FeishuAppID == "" && cfg.FeishuAppSecret == "" {
			fmt.Fprintln(os.Stdout, "")
			fmt.Fprint(os.Stdout, "飞书 App ID (直接回车跳过): ")
			appID, _ := reader.ReadString('\n')
			appID = strings.TrimSpace(appID)
			if appID == "" {
				fmt.Fprintln(os.Stdout, "已跳过飞书配置。")
				return
			}
			fmt.Fprint(os.Stdout, "飞书 App Secret: ")
			appSecret, _ := reader.ReadString('\n')
			appSecret = strings.TrimSpace(appSecret)
			_ = s.SetConfig(store.ConfigKeyFeishuAppID, appID)
			_ = s.SetConfig(store.ConfigKeyFeishuAppSecret, appSecret)
			fmt.Fprintln(os.Stdout, "正在校验飞书凭证…")
			if err := feishu.TestCredentials(appID, appSecret); err != nil {
				fmt.Fprintln(os.Stdout, "飞书校验失败:", err)
				continue
			}
			fmt.Fprintln(os.Stdout, "飞书校验通过。")
			return
		}
		if err := feishu.TestCredentials(cfg.FeishuAppID, cfg.FeishuAppSecret); err != nil {
			fmt.Fprintln(os.Stdout, "飞书校验失败:", err)
			fmt.Fprint(os.Stdout, "飞书 App ID (直接回车跳过): ")
			appID, _ := reader.ReadString('\n')
			appID = strings.TrimSpace(appID)
			if appID == "" {
				_ = s.SetConfig(store.ConfigKeyFeishuAppID, "")
				_ = s.SetConfig(store.ConfigKeyFeishuAppSecret, "")
				fmt.Fprintln(os.Stdout, "已清除飞书配置并跳过。")
				return
			}
			fmt.Fprint(os.Stdout, "飞书 App Secret: ")
			appSecret, _ := reader.ReadString('\n')
			appSecret = strings.TrimSpace(appSecret)
			_ = s.SetConfig(store.ConfigKeyFeishuAppID, appID)
			_ = s.SetConfig(store.ConfigKeyFeishuAppSecret, appSecret)
			if err := feishu.TestCredentials(appID, appSecret); err != nil {
				fmt.Fprintln(os.Stdout, "飞书校验仍失败:", err)
				continue
			}
			fmt.Fprintln(os.Stdout, "飞书校验通过。")
		} else {
			fmt.Fprintln(os.Stdout, "飞书校验通过。")
		}
		return
	}
}
