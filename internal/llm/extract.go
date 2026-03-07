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
	ScheduleInstruction  string `json:"schedule_instruction"`   // 任务内容
	ScheduleRunAt        string `json:"schedule_run_at"`         // 单个时间（daily/hourly/单次）
	ScheduleRunAtList    string `json:"schedule_run_at_list"`    // 多个每日时间，逗号分隔如 "06:00,11:30,17:30"
	ScheduleRepeat       string `json:"schedule_repeat"`          // "daily" | "hourly" | "interval" | ""
	ScheduleIntervalMins string `json:"schedule_interval_mins"`  // repeat="interval" 时填间隔分钟数，如 "270"（4.5小时）
	ScheduleID           string `json:"schedule_id"`              // schedule_delete/update 时填任务 id
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

	sys := `你是 WILL，运行在飞书上的个人助手。能力仅限：待办管理、定时任务、版本检查、普通对话/记忆、执行 shell 命令。不能访问外部网站、发邮件、接入第三方平台。

定时任务（schedule_add）字段说明：
- schedule_instruction：执行时系统会注入最新待办和对话，只需描述角色和任务，如「作为严厉导师，基于当前待办和对话，给出下一步建议」
- schedule_repeat："daily"（每天固定时间）| "hourly"（每小时）| "interval"（按间隔分钟数）| ""（单次）
- schedule_run_at：单个时间，daily 填 "09:00"，hourly/interval 可留空，单次填 ISO 如 "2025-03-07T09:00"
- schedule_run_at_list：多个每日时间，逗号分隔如 "06:00,11:30,17:30,21:00"（用户指定多个时间点时填此字段，同时 schedule_repeat="daily"）
- schedule_interval_mins：repeat="interval" 时填间隔分钟数，如 "270"（4.5小时）、"240"（4小时）

示例：用户说「每天6点和21点提醒」→ schedule_repeat="daily", schedule_run_at_list="06:00,21:00"
示例：用户说「每隔4-5小时」→ schedule_repeat="interval", schedule_interval_mins="270"

必须用纯 JSON 回复（不要其他文字）：
{"intent":"", "todo_title":"", "todo_id":"", "schedule_instruction":"", "schedule_run_at":"", "schedule_run_at_list":"", "schedule_repeat":"", "schedule_interval_mins":"", "schedule_id":"", "config":{}, "memory":{}, "command":"", "reply":""}

意图：todo_list/todo_add/todo_done/todo_delete/version_check/schedule_list/schedule_add/schedule_update/schedule_delete/空
config key 仅限：feishu_app_id, feishu_app_secret, llm_api_key, llm_base_url, llm_model, mode, port, bind, internal_token, worker_urls`

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

// CallForInstruction 执行定时任务指令：把实际待办列表和近期对话注入上下文，再由 LLM 分析并输出结论
func CallForInstruction(cfg *config.Config, userScope string, instruction string, s *store.Store) (string, error) {
	if cfg == nil || cfg.LLMApiKey == "" {
		return "", fmt.Errorf("未配置 LLM")
	}
	baseURL := strings.TrimRight(cfg.LLMBaseURL, "/")
	model := cfg.LLMModel
	if model == "" {
		model = "gpt-4o-mini"
	}

	// 构建上下文：从数据库拉取实际数据注入 system prompt，LLM 直接分析而非去"获取"
	var ctxBuf strings.Builder
	ctxBuf.WriteString("你是一个助手，正在自动执行定时任务。下方已提供所有相关数据，直接基于这些数据执行指令并输出简洁结论。不要 JSON，只输出给用户看的文字。\n")

	if s != nil {
		openID := strings.TrimPrefix(userScope, "user:")
		// 注入待办列表
		todos, err := s.ListTodos(openID)
		if err == nil && len(todos) > 0 {
			ctxBuf.WriteString("\n【当前待办列表】\n")
			for i, t := range todos {
				status := "未完成"
				if t.Status == "done" {
					status = "已完成"
				}
				ctxBuf.WriteString(fmt.Sprintf("%d. %s (%s)\n", i+1, t.Title, status))
			}
		} else {
			ctxBuf.WriteString("\n【当前待办列表】暂无待办。\n")
		}
		// 注入近期对话
		history, err := s.GetRecentConversation(openID, maxHistoryMessages)
		if err == nil && len(history) > 0 {
			ctxBuf.WriteString("\n【近期对话记录】\n")
			for _, m := range history {
				role := "用户"
				if m.Role == "assistant" {
					role = "助手"
				}
				ctxBuf.WriteString(role + ": " + truncateRunes(m.Content, maxHistoryRunes) + "\n")
			}
		}
		// 注入用户记忆
		mem, err := s.ListMemory(userScope)
		if err == nil && len(mem) > 0 {
			keys := make([]string, 0, len(mem))
			for k := range mem {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			ctxBuf.WriteString("\n【用户记忆】\n")
			for _, k := range keys {
				ctxBuf.WriteString(k + ": " + mem[k] + "\n")
			}
		}
	}

	sys := ctxBuf.String()
	messages := []map[string]string{
		{"role": "system", "content": sys},
		{"role": "user", "content": instruction},
	}
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
