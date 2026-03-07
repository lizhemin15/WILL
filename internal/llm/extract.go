package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
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

// Response 期望 LLM 返回的 JSON 结构；先由 intent 判定意图再分发
type Response struct {
	Intent    string            `json:"intent"`     // todo_list / todo_add / todo_done / todo_delete / version_check / 空或 chat 等
	TodoTitle string            `json:"todo_title"` // todo_add 时填待办内容
	TodoID    string            `json:"todo_id"`   // todo_done / todo_delete 时填待办 id（数字字符串）
	Config    map[string]string `json:"config"`
	Memory    map[string]string `json:"memory"`
	Command   string            `json:"command"`
	Reply     string            `json:"reply"`
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

// Call 调用 LLM，解析出 config/memory/command/reply；若 s 非空则从 SQLite 拉取该用户记忆作为上下文
func Call(cfg *config.Config, userScope string, userMessage string, s *store.Store) (Response, error) {
	var out Response
	if cfg.LLMApiKey == "" {
		return out, fmt.Errorf("未配置 LLM（OPENAI_API_KEY 或 llm_api_key）")
	}

	baseURL := strings.TrimRight(cfg.LLMBaseURL, "/")
	model := cfg.LLMModel
	if model == "" {
		model = "gpt-4o-mini"
	}

	sys := `你是 WILL 的助手。请先根据用户消息判断意图，再填对应字段。必须用纯 JSON 回复，只包含一个 JSON 对象，不要其他文字。

意图 intent 取值（只能填其中一个，否则留空）：
- todo_list：用户要查看待办列表（如「我的待办」「看看待办」「有什么待办」）
- todo_add：用户要添加待办，此时必填 todo_title（待办内容）
- todo_done：用户要把某条待办标为已完成，此时必填 todo_id（待办编号数字字符串，如 "1"）
- todo_delete：用户要删除某条待办，此时必填 todo_id
- version_check：用户要检查程序是否有新版本（如「检查更新」「有没有新版本」「查版本」）
- 留空或 chat：其他情况（执行命令、改配置、记记忆、普通对话）

JSON 格式（未用到的字段填空字符串或空对象）：
{"intent": "上述之一或空", "todo_title": "", "todo_id": "", "config": {}, "memory": {}, "command": "要执行的 shell 命令，若无则空", "reply": "给用户的简短回复"}

说明：config 的 key 仅限 feishu_app_id, feishu_app_secret, mode, internal_token, worker_urls, port, bind, llm_api_key, llm_base_url, llm_model。不要用 git 检查版本，用户说检查更新时 intent 填 version_check 即可。`

	if s != nil {
		mem, err := s.ListMemory(userScope)
		if err == nil && len(mem) > 0 {
			keys := make([]string, 0, len(mem))
			for k := range mem {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var lines []string
			for _, k := range keys {
				lines = append(lines, k+": "+mem[k])
			}
			sys += "\n\n当前用户记忆（可引用、需更新时在 memory 里返回）：\n" + strings.Join(lines, "\n")
		}
	}

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
		body, _ := io.ReadAll(resp.Body)
		msg := string(body)
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		if msg != "" {
			return out, fmt.Errorf("LLM 请求失败: %d — %s", resp.StatusCode, strings.TrimSpace(msg))
		}
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
