package feishu

import (
	"encoding/json"
	"strings"
)

// URL 校验：飞书订阅时 POST 的 body
type URLVerification struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	Token     string `json:"token"`
}

// 事件回调通用外壳（未加密时）
type EventEnvelope struct {
	Type      string          `json:"type"`
	Challenge string          `json:"challenge"`
	Token     string          `json:"token"`
	Header    *EventHeader    `json:"header,omitempty"`
	Event     json.RawMessage `json:"event,omitempty"`
}

type EventHeader struct {
	EventID    string `json:"event_id"`
	EventType  string `json:"event_type"`
	CreateTime string `json:"create_time"`
	Token      string `json:"token"`
	AppID      string `json:"app_id"`
}

const EventIMMessageReceive = "im.message.receive_v1"

type IMMessageEvent struct {
	Sender  *IMMessageSender  `json:"sender"`
	Message *IMMessageMessage `json:"message"`
}

type IMMessageSender struct {
	SenderID   *SenderID `json:"sender_id"`
	SenderType string    `json:"sender_type"`
	TenantKey  string    `json:"tenant_key"`
}

type SenderID struct {
	UnionID string `json:"union_id"`
	UserID  string `json:"user_id"`
	OpenID  string `json:"open_id"`
}

type IMMessageMessage struct {
	MessageID   string `json:"message_id"`
	RootID      string `json:"root_id"`
	ParentID    string `json:"parent_id"`
	CreateTime  string `json:"create_time"`
	ChatID      string `json:"chat_id"`
	ChatType    string `json:"chat_type"`
	MessageType string `json:"message_type"`
	Content     string `json:"content"`
}

// ParseTextContent 解析 content 中的 text 文本（content 为 JSON 字符串）
func ParseTextContent(content string) string {
	var c struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal([]byte(content), &c)
	return strings.TrimSpace(c.Text)
}

// IsAllowed 检查 open_id 是否在白名单中；白名单为空则不允许任何人（安全默认）
func IsAllowed(openID string, allowed []string) bool {
	if openID == "" {
		return false
	}
	for _, a := range allowed {
		if a == openID {
			return true
		}
	}
	return false
}
