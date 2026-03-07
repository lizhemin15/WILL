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

const (
	maxHistoryMessages = 20
	maxHistoryRunes    = 500
	maxToolRounds      = 5
)

// llmClient 独立 HTTP 客户端，超时与全局 DefaultClient 隔离
var llmClient = &http.Client{Timeout: 120 * time.Second}

// ToolExecutor 执行单个工具调用，返回结果字符串
type ToolExecutor func(name string, argsJSON []byte) string

// rawToolCall OpenAI tool_calls 结构
type rawToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// message 用于构造和传递 LLM 消息历史
type message struct {
	Role       string        `json:"role"`
	Content    string        `json:"content,omitempty"`
	ToolCalls  []rawToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	Name       string        `json:"name,omitempty"`
}

// AllowedConfigKeys 允许通过 LLM/命令修改的配置键
var AllowedConfigKeys = map[string]string{
	"feishu_app_id":        store.ConfigKeyFeishuAppID,
	"feishu_app_secret":    store.ConfigKeyFeishuAppSecret,
	"llm_api_key":          store.ConfigKeyLLMApiKey,
	"llm_base_url":         store.ConfigKeyLLMBaseURL,
	"llm_model":            store.ConfigKeyLLMModel,
	"mode":                 store.ConfigKeyMode,
	"port":                 store.ConfigKeyPort,
	"bind":                 store.ConfigKeyBind,
	"internal_token":       store.ConfigKeyInternalToken,
	"worker_urls":          store.ConfigKeyWorkerURLs,
	"timezone":             store.ConfigKeyTimezone,
	"feishu_subscribe_mode": store.ConfigKeyFeishuSubscribeMode,
}

// PendingConfigKey 存在 memory 中的待确认配置变更键
const PendingConfigKey = "_pending_config"

// TestConfig 校验 LLM 配置是否可用
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
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.LLMApiKey)
	resp, err := llmClient.Do(req)
	if err != nil {
		return fmt.Errorf("LLM 连接失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("LLM 请求失败: %d", resp.StatusCode)
	}
	return nil
}

// CallChat 发一次简单对话补全，返回文本（供 orchestrator 使用）
func CallChat(cfg *config.Config, systemPrompt, userContent string) (string, error) {
	if cfg == nil || cfg.LLMApiKey == "" {
		return "", fmt.Errorf("未配置 LLM")
	}
	baseURL := strings.TrimRight(cfg.LLMBaseURL, "/")
	model := cfg.LLMModel
	if model == "" {
		model = "gpt-4o-mini"
	}
	msgs := []map[string]string{
		{"role": "system", "content": systemPrompt},
		{"role": "user", "content": userContent},
	}
	body := map[string]interface{}{"model": model, "messages": msgs}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.LLMApiKey)
	resp, err := llmClient.Do(req)
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
			Message struct{ Content string `json:"content"` } `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &apiResp); err != nil || len(apiResp.Choices) == 0 {
		return "", fmt.Errorf("LLM 未返回有效内容")
	}
	return strings.TrimSpace(apiResp.Choices[0].Message.Content), nil
}

// Call 主要 LLM 调用入口：使用 Function Calling，内部处理工具调用循环
// executor 负责执行工具并返回结果字符串
func Call(cfg *config.Config, scope, userMessage string, s *store.Store, executor ToolExecutor) (string, error) {
	if cfg == nil || cfg.LLMApiKey == "" {
		return "", fmt.Errorf("未配置 LLM（OPENAI_API_KEY 或 llm_api_key）")
	}
	baseURL := strings.TrimRight(cfg.LLMBaseURL, "/")
	model := cfg.LLMModel
	if model == "" {
		model = "gpt-4o-mini"
	}

	// 构建 system prompt
	sys := buildSystemPrompt(cfg, scope, s)

	// 初始消息列表
	msgs := []message{{Role: "system", Content: sys}}

	// 注入对话历史
	if s != nil {
		openID := strings.TrimPrefix(scope, "user:")
		history, _ := s.GetRecentConversation(openID, maxHistoryMessages)
		for _, m := range history {
			msgs = append(msgs, message{
				Role:    m.Role,
				Content: truncateRunes(m.Content, maxHistoryRunes),
			})
		}
	}
	msgs = append(msgs, message{Role: "user", Content: userMessage})

	// 工具调用循环
	for round := 0; round < maxToolRounds; round++ {
		bodyMap := map[string]interface{}{
			"model":       model,
			"messages":    msgs,
			"tools":       toolDefs,
			"tool_choice": "auto",
		}
		bodyBytes, _ := json.Marshal(bodyMap)

		req, err := http.NewRequest(http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(bodyBytes))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+cfg.LLMApiKey)

		resp, err := llmClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("LLM 连接失败: %w", err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			msg := string(raw)
			if len(msg) > 300 {
				msg = msg[:300] + "..."
			}
			return "", fmt.Errorf("LLM 请求失败: %d — %s", resp.StatusCode, strings.TrimSpace(msg))
		}

		var apiResp struct {
			Choices []struct {
				Message struct {
					Content   string        `json:"content"`
					ToolCalls []rawToolCall `json:"tool_calls"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(raw, &apiResp); err != nil || len(apiResp.Choices) == 0 {
			return "", fmt.Errorf("LLM 未返回有效内容")
		}

		choice := apiResp.Choices[0]

		// 没有工具调用 → 返回文本
		if len(choice.Message.ToolCalls) == 0 {
			return strings.TrimSpace(choice.Message.Content), nil
		}

		// 有工具调用 → 执行并追加结果
		assistantMsg := message{
			Role:      "assistant",
			ToolCalls: choice.Message.ToolCalls,
		}
		if choice.Message.Content != "" {
			assistantMsg.Content = choice.Message.Content
		}
		msgs = append(msgs, assistantMsg)

		for _, tc := range choice.Message.ToolCalls {
			result := ""
			if executor != nil {
				result = executor(tc.Function.Name, []byte(tc.Function.Arguments))
			}
			msgs = append(msgs, message{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
			})
		}
	}

	// 超过最大轮数，做最后一次不带工具的调用
	bodyMap := map[string]interface{}{"model": model, "messages": msgs}
	bodyBytes, _ := json.Marshal(bodyMap)
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.LLMApiKey)
	resp, err := llmClient.Do(req)
	if err != nil {
		return "", err
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var final struct {
		Choices []struct {
			Message struct{ Content string `json:"content"` } `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &final); err != nil || len(final.Choices) == 0 {
		return "已处理完成。", nil
	}
	return strings.TrimSpace(final.Choices[0].Message.Content), nil
}

// CallForInstruction 定时任务执行：注入用户实际数据（待办/对话/记忆），直接输出文字结果
func CallForInstruction(cfg *config.Config, scope, instruction string, s *store.Store) (string, error) {
	if cfg == nil || cfg.LLMApiKey == "" {
		return "", fmt.Errorf("未配置 LLM")
	}
	baseURL := strings.TrimRight(cfg.LLMBaseURL, "/")
	model := cfg.LLMModel
	if model == "" {
		model = "gpt-4o-mini"
	}

	var ctxBuf strings.Builder
	ctxBuf.WriteString("你正在执行一个定时任务。以下是用户的最新数据，直接基于这些数据执行指令，输出给用户看的文字（不要 JSON，不要菜单选项）。\n")

	if s != nil {
		openID := strings.TrimPrefix(scope, "user:")
		todos, _ := s.ListTodos(openID)
		ctxBuf.WriteString("\n【当前待办列表】\n")
		if len(todos) == 0 {
			ctxBuf.WriteString("（无待办）\n")
		} else {
			for i, t := range todos {
				status := "未完成"
				if t.Status == "done" {
					status = "已完成"
				}
				ctxBuf.WriteString(fmt.Sprintf("%d. %s (%s)\n", i+1, t.Title, status))
			}
		}

		history, _ := s.GetRecentConversation(openID, 10)
		ctxBuf.WriteString("\n【近期对话记录】\n")
		if len(history) == 0 {
			ctxBuf.WriteString("（无记录）\n")
		} else {
			for _, m := range history {
				role := "用户"
				if m.Role == "assistant" {
					role = "助手"
				}
				ctxBuf.WriteString(role + ": " + truncateRunes(m.Content, 200) + "\n")
			}
		}

		mem, _ := s.ListMemory(scope)
		if len(mem) > 0 {
			ctxBuf.WriteString("\n【用户记忆】\n")
			keys := make([]string, 0, len(mem))
			for k := range mem {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				ctxBuf.WriteString(k + ": " + mem[k] + "\n")
			}
		}
	}

	sysPart := ctxBuf.String()
	msgs := []map[string]string{
		{"role": "system", "content": sysPart},
		{"role": "user", "content": instruction},
	}
	body := map[string]interface{}{"model": model, "messages": msgs}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.LLMApiKey)
	resp, err := llmClient.Do(req)
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
			Message struct{ Content string `json:"content"` } `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &apiResp); err != nil || len(apiResp.Choices) == 0 {
		return "", fmt.Errorf("LLM 未返回有效内容")
	}
	return strings.TrimSpace(apiResp.Choices[0].Message.Content), nil
}

// buildSystemPrompt 构建主系统提示词，注入记忆上下文
func buildSystemPrompt(cfg *config.Config, scope string, s *store.Store) string {
	tz := "Asia/Shanghai"
	if cfg != nil && cfg.Timezone != "" {
		tz = cfg.Timezone
	}
	loc, _ := time.LoadLocation(tz)
	if loc == nil {
		loc = time.UTC
	}
	now := time.Now().In(loc).Format("2006-01-02 15:04 (Mon) MST")

	sys := `你是 WILL，运行在飞书上的个人助手。
- 使用工具完成待办、定时任务、版本检查、记忆等操作；普通对话直接文字回复
- 直接执行用户意图，不要输出选项菜单或让用户"回复某某"
- 用户指定多个定时时间点时，多次调用 schedule_add 工具（每个时间点一次）
- 当前时间：` + now

	if s != nil {
		mem, err := s.ListMemory(scope)
		if err == nil && len(mem) > 0 {
			keys := make([]string, 0, len(mem))
			for k := range mem {
				if k == PendingConfigKey {
					continue
				}
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if len(keys) > 0 {
				sys += "\n\n【用户记忆】"
				for _, k := range keys {
					sys += "\n" + k + ": " + mem[k]
				}
			}
		}
	}
	return sys
}

func truncateRunes(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}
