// 飞书长连接（WebSocket）接收事件。需配置 FEISHU_SUBSCRIBE_MODE=ws 并安装 github.com/larksuite/oapi-sdk-go/v3

package feishu

import (
	"context"
	"log"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/larksuite/oapi-sdk-go/v3/ws"
)

// ProcessMessageFunc 处理一条消息，返回要回复的文案及是否发送回复
type ProcessMessageFunc func(openID, messageID, content string) (reply string, sendReply bool)

// StartWSClient 启动飞书长连接客户端，收到消息时调用 onMessage，并在需要时用 ReplyMessage 回复（依赖已调用的 InitClient）
func StartWSClient(appID, appSecret string, onMessage ProcessMessageFunc) {
	eventHandler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
			openID, messageID, content := extractP2Message(event)
			log.Printf("[feishu] 收到消息 event: open_id=%q message_id=%q content_len=%d", openID, messageID, len(content))
			if messageID == "" {
				log.Printf("[feishu] 跳过: message_id 为空")
				return nil
			}
			text := ParseTextContent(content)
			if text == "" {
				log.Printf("[feishu] 跳过: 解析文本为空 (content=%q)", content)
				return nil
			}
			log.Printf("[feishu] 处理文本: %q", text)
			// 异步处理并回复，避免 LLM 调用超过飞书 3 秒限制导致超时
			go func() {
				reply, sendReply := onMessage(openID, messageID, text)
				if sendReply && reply != "" {
					if err := ReplyMessage(messageID, reply); err != nil {
						log.Printf("[feishu] 回复失败: %v", err)
					} else {
						log.Printf("[feishu] 已回复 message_id=%q", messageID)
					}
				} else {
					log.Printf("[feishu] 本条不回复 sendReply=%v reply_empty=%v", sendReply, reply == "")
				}
			}()
			return nil
		})
	cli := ws.NewClient(appID, appSecret,
		ws.WithEventHandler(eventHandler),
		ws.WithLogLevel(larkcore.LogLevelInfo), // 可改为 LogLevelError 减少日志
	)
	log.Printf("WILL: 飞书长连接启动中…（请在开放平台「事件订阅」里添加「接收消息」事件并保存）")
	if err := cli.Start(context.Background()); err != nil {
		log.Printf("WILL: 飞书长连接异常退出: %v", err)
	}
}

func extractP2Message(event *larkim.P2MessageReceiveV1) (openID, messageID, content string) {
	if event == nil {
		return "", "", ""
	}
	e := event.Event
	if e == nil {
		return "", "", ""
	}
	if e.Message != nil {
		if e.Message.MessageId != nil {
			messageID = *e.Message.MessageId
		}
		if e.Message.Content != nil {
			content = *e.Message.Content
		}
	}
	if e.Sender != nil && e.Sender.SenderId != nil && e.Sender.SenderId.OpenId != nil {
		openID = *e.Sender.SenderId.OpenId
	}
	return openID, messageID, content
}
