package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/yourusername/will/internal/config"
	"github.com/yourusername/will/internal/store"
)

// AllowedConfigKeys 允许通过 LLM 写入的配置键（与 store 一致）
var AllowedConfigKeys = map[string]string{
	"feishu_app_id":     store.ConfigKeyFeishuAppID,
	"feishu_app_secret": store.ConfigKeyFeishuAppSecret,
	"mode":              store.ConfigKeyMode,
	"internal_token":    store.ConfigKeyInternalToken,
	"worker_urls":       store.ConfigKeyWorkerURLs,
	"port":              store.ConfigKeyPort,
	"bind":              store.ConfigKeyBind,
	"llm_api_key":       store.ConfigKeyLLMApiKey,
	"llm_base_url":      store.ConfigKeyLLMBaseURL,
	"llm_model":         store.ConfigKeyLLMModel,
}

// Response 期望 LLM 返回的 JSON 结构
type Response struct {
	Config  map[string]string `json:"config"`
	Memory  map[string]string `json:"memory"`
	Command string            `json:"command"`
	Reply   string            `json:"reply"`
}

// TestConfig 校验 LLM 配置是否可用（发一次最小 completion 请求）
func TestConfig(cfg *config.Config) error {
	if cfg == nil || cfg.LLMApiKey == "" {
		return fmt.Errorf("未配置 LLM API Key")
	}
	baseURL := strings.TrimRight(cfg.LLMBaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	model := cfg.LLMModel
	if model == "" {
		model = "gpt-4o-mini"
	}
	body := map[string]interface{}{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"max_tokens": 1,
	}
	bodyBytes, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.LLMApiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("LLM 连接失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("LLM 请求失败: %d", resp.StatusCode)
	}
	return nil
}

// Call 调用 LLM，解析出 config/memory/command/reply
func Call(cfg *config.Config, userScope string, userMessage string) (Response, error) {
	var out Response
	if cfg.LLMApiKey == "" {
		return out, fmt.Errorf("未配置 LLM（OPENAI_API_KEY 或 llm_api_key）")
	}

	baseURL := strings.TrimRight(cfg.LLMBaseURL, "/")
	model := cfg.LLMModel
	if model == "" {
		model = "gpt-4o-mini"
	}

	sys := `你是 WILL 的助手。用户可能要求：1) 保存配置（飞书 app_id、app_secret、mode、worker_urls 等）；2) 记录或读取记忆；3) 执行系统命令；4) 普通对话。
你必须用纯 JSON 回复，且只包含一个 JSON 对象，不要其他文字。格式：
{"config": {"key": "value", ...}, "memory": {"key": "value", ...}, "command": "要执行的 shell 命令，若无则空字符串", "reply": "给用户的简短回复"}
说明：config 的 key 仅限：feishu_app_id, feishu_app_secret, mode, internal_token, worker_urls, port, bind, llm_api_key, llm_base_url, llm_model。memory 会按用户维度存储。若用户只是闲聊或无需执行/保存，command 和 config/memory 可为空。`

	body := map[string]interface{}{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": sys},
			{"role": "user", "content": userMessage},
		},
		"max_tokens": 1024,
	}
	bodyBytes, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.LLMApiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("LLM 请求失败: %d", resp.StatusCode)
	}

	var apiResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return out, err
	}
	if len(apiResp.Choices) == 0 {
		return out, fmt.Errorf("LLM 未返回内容")
	}
	content := strings.TrimSpace(apiResp.Choices[0].Message.Content)
	content = extractJSON(content)
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		return out, fmt.Errorf("解析 LLM 返回的 JSON 失败: %w", err)
	}
	return out, nil
}

// Apply 将 LLM 返回的 config/memory 写入 store，并返回要执行的 command 和回复文案
func Apply(s *store.Store, openID string, r Response) (command string, reply string) {
	if s == nil {
		return r.Command, r.Reply
	}
	for k, v := range r.Config {
		if storeKey, ok := AllowedConfigKeys[strings.ToLower(strings.TrimSpace(k))]; ok {
			_ = s.SetConfig(storeKey, strings.TrimSpace(v))
		}
	}
	scope := "user:" + openID
	for k, v := range r.Memory {
		_ = s.SetMemory(scope, strings.TrimSpace(k), strings.TrimSpace(v))
	}
	return strings.TrimSpace(r.Command), r.Reply
}

func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSpace(s)
	}
	start := strings.Index(s, "{")
	if start < 0 {
		return s
	}
	end := strings.LastIndex(s, "}")
	if end < start {
		return s[start:]
	}
	return s[start : end+1]
}
