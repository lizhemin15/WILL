// 飞书长连接（WebSocket）接收事件。需配置 FEISHU_SUBSCRIBE_MODE=ws 并安装 github.com/larksuite/oapi-sdk-go/v3

package feishu

import (
	"context"
	"log"

	"github.com/larksuite/oapi-sdk-go/v3/core"
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
			if messageID == "" {
				return nil
			}
			text := ParseTextContent(content)
			if text == "" {
				return nil
			}
			reply, sendReply := onMessage(openID, messageID, text)
			if sendReply && reply != "" {
				if err := ReplyMessage(messageID, reply); err != nil {
					log.Printf("feishu ws reply error: %v", err)
				}
			}
			return nil
		})
	cli := ws.NewClient(appID, appSecret,
		ws.WithEventHandler(eventHandler),
		ws.WithLogLevel(core.LogLevelError),
	)
	log.Printf("WILL: 飞书长连接启动中…")
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
