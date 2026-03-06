package setup

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/yourusername/will/internal/config"
	"github.com/yourusername/will/internal/store"
)

const urlHint = "只填到域名或 /v1，不要加 /chat/completions 等路径，程序会自动拼接。例如: https://api.openai.com/v1"

// PromptLLMIfMissing 若 SQLite 中无 LLM 配置，则在命令行与用户交互并写入
func PromptLLMIfMissing(s *store.Store) *config.Config {
	if s == nil {
		return nil
	}
	_, hasKey := s.GetConfig(store.ConfigKeyLLMApiKey)
	if hasKey {
		return nil
	}
	// 环境变量已存在则不再交互
	if os.Getenv("OPENAI_API_KEY") != "" {
		return nil
	}

	fmt.Fprintln(os.Stdout, "WILL 检测到尚未配置 LLM，请在下方输入（也可通过环境变量 OPENAI_* 或 /setup 页面配置后重启）。")
	fmt.Fprintln(os.Stdout, "")

	reader := bufio.NewReader(os.Stdin)

	// API Key
	fmt.Fprint(os.Stdout, "LLM API Key (必填): ")
	key, _ := reader.ReadString('\n')
	key = strings.TrimSpace(key)
	if key == "" {
		fmt.Fprintln(os.Stdout, "未输入 API Key，退出。")
		os.Exit(1)
	}

	// Base URL
	fmt.Fprintf(os.Stdout, "LLM Base URL (%s): ", urlHint)
	baseURL, _ := reader.ReadString('\n')
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	baseURL = strings.TrimRight(baseURL, "/")

	// Model
	fmt.Fprint(os.Stdout, "LLM Model (直接回车则用 gpt-4o-mini): ")
	model, _ := reader.ReadString('\n')
	model = strings.TrimSpace(model)
	if model == "" {
		model = "gpt-4o-mini"
	}

	_ = s.SetConfig(store.ConfigKeyLLMApiKey, key)
	_ = s.SetConfig(store.ConfigKeyLLMBaseURL, baseURL)
	_ = s.SetConfig(store.ConfigKeyLLMModel, model)
	fmt.Fprintln(os.Stdout, "")
	fmt.Fprintln(os.Stdout, "已写入本地数据库，后续将直接使用。")
	return config.LoadFromStore(s)
}
