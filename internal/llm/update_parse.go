package llm

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/yourusername/will/internal/config"
)

// UpdateIntent 用户对「是否更新」的意图
type UpdateIntent struct {
	Action      string `json:"action"`       // "now" | "later" | "skip" | ""
	RemindHours int    `json:"remind_hours"` // 稍后提醒时的小时数，0 表示默认 24
}

var reHours = regexp.MustCompile(`(\d+)\s*小时`)

// ParseUpdateReply 用关键词匹配解析用户对「是否更新」的回复，无需 LLM
// 返回 action="" 表示消息与更新无关，调用方应跳过
func ParseUpdateReply(_ *config.Config, userReply string) (UpdateIntent, error) {
	t := strings.TrimSpace(userReply)
	lower := strings.ToLower(t)

	// 先判断是否与更新相关：若不含任何更新关键词，直接返回空意图
	updateKeywords := []string{
		"更新", "升级", "立即", "马上", "现在", "好的", "可以", "确定", "同意",
		"稍后", "以后", "等下", "一会", "再说", "小时后", "明天",
		"不", "不用", "暂不", "跳过", "取消", "算了", "不要",
	}
	relevant := false
	for _, k := range updateKeywords {
		if strings.Contains(lower, k) {
			relevant = true
			break
		}
	}
	if !relevant {
		return UpdateIntent{}, nil
	}

	// 立即更新
	nowWords := []string{"立即更新", "立刻更新", "马上更新", "现在更新", "立即", "立刻", "马上", "现在更新", "更新吧", "好的", "好", "可以", "确定", "同意", "更新", "升级"}
	for _, w := range nowWords {
		if lower == w || strings.HasPrefix(lower, w) {
			return UpdateIntent{Action: "now"}, nil
		}
	}

	// 明确跳过
	skipWords := []string{"不用", "不要", "暂不", "跳过", "取消", "算了", "不更新", "不升级", "不", "否"}
	for _, w := range skipWords {
		if lower == w || lower == w+"了" {
			return UpdateIntent{Action: "skip"}, nil
		}
	}

	// 稍后提醒：解析小时数
	if m := reHours.FindStringSubmatch(lower); len(m) == 2 {
		h, _ := strconv.Atoi(m[1])
		if h <= 0 {
			h = 24
		}
		return UpdateIntent{Action: "later", RemindHours: h}, nil
	}
	laterWords := []string{"稍后", "以后", "一会", "等下", "再说", "明天", "等等", "过会"}
	for _, w := range laterWords {
		if strings.Contains(lower, w) {
			return UpdateIntent{Action: "later", RemindHours: 24}, nil
		}
	}

	// 无法判断，返回空（不处理）
	return UpdateIntent{}, nil
}
