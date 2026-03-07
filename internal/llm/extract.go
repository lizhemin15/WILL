package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yourusername/will/internal/config"
	"github.com/yourusername/will/internal/store"
)

const maxHistoryRunes = 400 // 注入 LLM 的每条历史消息最大长度，防 token 爆炸
const maxHistoryMessages = 10 // 最近对话轮数（约 5 轮）

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
	Intent    string            `json:"intent"`
	TodoTitle string            `json:"todo_title"`
	TodoID    string            `json:"todo_id"`
	// 定时任务：schedule_list / schedule_add / schedule_delete / schedule_update
	ScheduleInstruction string `json:"schedule_instruction"` // 任务内容（可含：先查待办、查对话、搜最新信息等）
	ScheduleRunAt       string `json:"schedule_run_at"`      // 执行时间：ISO 如 2025-03-07T09:00:00，或每日时间 09:00
	ScheduleRepeat      string `json:"schedule_repeat"`       // "daily" 或空
	ScheduleID          string `json:"schedule_id"`           // schedule_delete/update 时填任务 id（数字字符串）
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

// CallChat 发一次对话补全，返回首条回复内容（供规划/审查等用）
func CallChat(cfg *config.Config, systemPrompt, userContent string) (string, error) {
	if cfg == nil || cfg.LLMApiKey == "" {
		return "", fmt.Errorf("未配置 LLM")
	}
	baseURL := strings.TrimRight(cfg.LLMBaseURL, "/")
	model := cfg.LLMModel
	if model == "" {
		model = "gpt-4o-mini"
	}
	messages := []map[string]string{
		{"role": "system", "content": systemPrompt},
		{"role": "user", "content": userContent},
	}
	body := map[string]interface{}{"model": model, "messages": messages, "max_tokens": 2048}
	bodyBytes, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.LLMApiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("LLM 请求失败: %d", resp.StatusCode)
	}
	var apiResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &apiResp); err != nil || len(apiResp.Choices) == 0 {
		return "", fmt.Errorf("LLM 未返回有效内容")
	}
	return strings.TrimSpace(apiResp.Choices[0].Message.Content), nil
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
- todo_list：查看待办列表。todo_add 必填 todo_title。todo_done / todo_delete 必填 todo_id：为列表中序号（从1开始），多条用逗号分隔如 "1,2,3"
- version_check：检查程序新版本
- schedule_list：用户要查看自己的定时任务列表
- schedule_add：用户要添加定时任务，必填 schedule_instruction（任务说明，如：先查待办再搜今日科技新闻）、schedule_run_at（执行时间，ISO 如 2025-03-07T09:00:00 或每日时间 09:00）、schedule_repeat（填 "daily" 或空）
- schedule_delete：用户要删除某条定时任务，必填 schedule_id（任务 id 数字字符串）
- schedule_update：用户要修改某条定时任务，必填 schedule_id，以及要改的 schedule_instruction / schedule_run_at / schedule_repeat
- 留空或 chat：其他情况（执行命令、改配置、记记忆、普通对话）

JSON 格式（未用到的字段填空字符串或空对象）：
{"intent": "上述之一或空", "todo_title": "", "todo_id": "", "schedule_instruction": "", "schedule_run_at": "", "schedule_repeat": "", "schedule_id": "", "config": {}, "memory": {}, "command": "", "reply": ""}

说明：config 的 key 仅限 feishu_app_id, feishu_app_secret, mode, internal_token, worker_urls, port, bind, llm_api_key, llm_base_url, llm_model。不要用 git 检查版本，用户说检查更新时 intent 填 version_check。定时任务内容可包含：查对话记录、查待办、搜索最新信息等。`

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

	messages := []map[string]string{{"role": "system", "content": sys}}
	if s != nil {
		openID := strings.TrimPrefix(userScope, "user:")
		history, err := s.GetRecentConversation(openID, maxHistoryMessages)
		if err == nil && len(history) > 0 {
			for _, m := range history {
				content := truncateRunes(m.Content, maxHistoryRunes)
				messages = append(messages, map[string]string{"role": m.Role, "content": content})
			}
		}
	}
	messages = append(messages, map[string]string{"role": "user", "content": userMessage})

	body := map[string]interface{}{
		"model":           model,
		"messages":        messages,
		"max_tokens":      1024,
		"response_format": map[string]string{"type": "json_object"},
	}
	bodyBytes, _ := json.Marshal(body)

	const maxParseRetries = 3
	var lastRaw string
	for attempt := 0; attempt < maxParseRetries; attempt++ {
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
		raw, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return out, readErr
		}
		if resp.StatusCode != http.StatusOK {
			// 部分提供商不支持 response_format，降级重试（去掉 response_format）
			if resp.StatusCode == 400 || resp.StatusCode == 422 {
				bodyFallback := map[string]interface{}{
					"model":      model,
					"messages":   messages,
					"max_tokens": 1024,
				}
				bodyBytes, _ = json.Marshal(bodyFallback)
				continue
			}
			msg := string(raw)
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
		if err := json.Unmarshal(raw, &apiResp); err != nil {
			return out, err
		}
		if len(apiResp.Choices) == 0 {
			return out, fmt.Errorf("LLM 未返回内容")
		}
		content := strings.TrimSpace(apiResp.Choices[0].Message.Content)
		lastRaw = content
		jsonStr := extractJSON(content)
		if err := json.Unmarshal([]byte(jsonStr), &out); err == nil {
			return out, nil
		}
		// 解析失败：短暂等待后重试
		if attempt < maxParseRetries-1 {
			time.Sleep(400 * time.Millisecond)
		}
	}
	// 全部重试失败：将原始内容作为 reply 降级返回，而非报错
	out.Reply = lastRaw
	if out.Reply == "" {
		out.Reply = "已处理，但无法解析结构化结果。"
	}
	return out, nil
}

// CallForInstruction 执行一条指令并返回纯文本回复（用于定时任务等）；若指令涉及搜索最新信息则自动使用搜索模型
func CallForInstruction(cfg *config.Config, userScope string, instruction string, s *store.Store) (string, error) {
	if cfg == nil || cfg.LLMApiKey == "" {
		return "", fmt.Errorf("未配置 LLM")
	}
	baseURL := strings.TrimRight(cfg.LLMBaseURL, "/")
	model := cfg.LLMModel
	if model == "" {
		model = "gpt-4o-mini"
	}
	sys := "你是执行定时任务的助手。请根据用户指令执行（可结合对话记录、待办、或搜索最新信息），然后给出简洁汇总回复。只输出结果内容，不要 JSON。"
	messages := []map[string]string{{"role": "system", "content": sys}}
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
			sys += "\n\n当前用户记忆：\n" + strings.Join(lines, "\n")
			messages[0]["content"] = sys
		}
		openID := strings.TrimPrefix(userScope, "user:")
		history, err := s.GetRecentConversation(openID, maxHistoryMessages)
		if err == nil && len(history) > 0 {
			for _, m := range history {
				messages = append(messages, map[string]string{"role": m.Role, "content": truncateRunes(m.Content, maxHistoryRunes)})
			}
		}
	}
	messages = append(messages, map[string]string{"role": "user", "content": instruction})
	body := map[string]interface{}{"model": model, "messages": messages, "max_tokens": 1024}
	bodyBytes, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.LLMApiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("LLM 请求失败: %d", resp.StatusCode)
	}
	var apiResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &apiResp); err != nil || len(apiResp.Choices) == 0 {
		return "", fmt.Errorf("LLM 未返回有效内容")
	}
	return strings.TrimSpace(apiResp.Choices[0].Message.Content), nil
}

// PendingConfigKey 存在此 key 时表示有待用户确认的配置变更（值为 JSON map[storeKey]value）
const PendingConfigKey = "pending_config"

// Apply 将 LLM 返回的 config/memory 写入 store；配置类修改不直接生效，写入待确认，需用户回复「确认」后才生效
func Apply(s *store.Store, openID string, r Response) (command string, reply string) {
	if s == nil {
		return r.Command, r.Reply
	}
	scope := "user:" + openID
	// 配置变更：一律进入待确认，不直接写 SetConfig
	if len(r.Config) > 0 {
		pending := make(map[string]string)
		var keys []string
		for k, v := range r.Config {
			k = strings.ToLower(strings.TrimSpace(k))
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			storeKey, ok := AllowedConfigKeys[k]
			if !ok {
				continue
			}
			pending[storeKey] = v
			keys = append(keys, k)
		}
		if len(pending) > 0 {
			sort.Strings(keys)
			jsonBytes, _ := json.Marshal(pending)
			_ = s.SetMemory(scope, PendingConfigKey, string(jsonBytes))
			return "", "将修改以下配置，请回复「确认」生效或「取消」忽略：" + strings.Join(keys, "、")
		}
	}
	// 无配置变更时，照常写 memory
	for k, v := range r.Memory {
		_ = s.SetMemory(scope, strings.TrimSpace(k), strings.TrimSpace(v))
	}
	return strings.TrimSpace(r.Command), r.Reply
}

func truncateRunes(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}

// extractJSON 从字符串中提取第一个完整的 JSON 对象（用括号计数，避免截断/截错）
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	// 去掉 markdown 代码块包裹
	for _, fence := range []string{"```json", "```"} {
		if strings.HasPrefix(s, fence) {
			s = strings.TrimPrefix(s, fence)
			if idx := strings.LastIndex(s, "```"); idx > 0 {
				s = s[:idx]
			}
			s = strings.TrimSpace(s)
			break
		}
	}
	start := strings.Index(s, "{")
	if start < 0 {
		return s
	}
	depth, i := 0, start
	runes := []rune(s)
	inStr, escape := false, false
	for i < len(runes) {
		ch := runes[i]
		if escape {
			escape = false
			i++
			continue
		}
		if ch == '\\' && inStr {
			escape = true
			i++
			continue
		}
		if ch == '"' {
			inStr = !inStr
			i++
			continue
		}
		if !inStr {
			if ch == '{' {
				depth++
			} else if ch == '}' {
				depth--
				if depth == 0 {
					return string(runes[start : i+1])
				}
			}
		}
		i++
	}
	// 未找到完整对象，返回从 start 到末尾（可能是截断的，调用方会重试）
	return string(runes[start:])
}
