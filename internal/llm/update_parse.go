package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/yourusername/will/internal/config"
)

// UpdateIntent 用户对「是否更新」的意图
type UpdateIntent struct {
	Action       string `json:"action"`        // "now" | "later" | "skip"
	RemindHours  int    `json:"remind_hours"`  // 稍后提醒时的小时数，0 表示默认 24
}

// ParseUpdateReply 用 LLM 解析用户对更新询问的回复
func ParseUpdateReply(cfg *config.Config, userReply string) (UpdateIntent, error) {
	var out UpdateIntent
	if cfg.LLMApiKey == "" {
		return out, fmt.Errorf("未配置 LLM")
	}
	baseURL := strings.TrimRight(cfg.LLMBaseURL, "/")
	model := cfg.LLMModel
	if model == "" {
		model = "gpt-4o-mini"
	}
	sys := `用户刚才收到 WILL 的更新提醒（发现新版本），现在回复了一句话。请判断用户意图，用纯 JSON 回复一个对象，不要其他文字。
格式：{"action":"now"|"later"|"skip", "remind_hours": 数字}
- action="now"：立即更新（如：好、更新、马上、可以）
- action="later"：稍后提醒（如：再说、一会、X小时后、明天；若提到具体时间则解析为 remind_hours，否则 24）
- action="skip"：本次不更新（如：不用、暂不、跳过）`

	body := map[string]interface{}{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": sys},
			{"role": "user", "content": userReply},
		},
		"max_tokens": 128,
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
		return out, fmt.Errorf("LLM %d", resp.StatusCode)
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
	content := extractJSON(strings.TrimSpace(apiResp.Choices[0].Message.Content))
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		return out, err
	}
	if out.Action == "" {
		out.Action = "skip"
	}
	if out.Action == "later" && out.RemindHours <= 0 {
		out.RemindHours = 24
	}
	return out, nil
}
