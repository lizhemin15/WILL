package feishu

import (
	"context"
	"fmt"
	"sync"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

var (
	globalClient *lark.Client
	clientMu     sync.RWMutex
)

// InitClient 使用 appID、appSecret 初始化全局飞书 SDK 客户端，回复与发消息均通过该客户端
func InitClient(appID, appSecret string) {
	clientMu.Lock()
	defer clientMu.Unlock()
	globalClient = lark.NewClient(appID, appSecret)
}

func getClient() *lark.Client {
	clientMu.RLock()
	defer clientMu.RUnlock()
	return globalClient
}

// ReplyMessage 回复指定消息（通过 SDK Im.Message.Reply）
func ReplyMessage(messageID, text string) error {
	cli := getClient()
	if cli == nil {
		return fmt.Errorf("feishu client not initialized")
	}
	content := larkim.NewTextMsgBuilder().Text(text).Build()
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			Content(content).
			MsgType(larkim.MsgTypeText).
			Build()).
		Build()
	_, err := cli.Im.Message.Reply(context.Background(), req)
	if err != nil {
		return fmt.Errorf("feishu reply: %w", err)
	}
	return nil
}

// SendMessageToUser 主动给用户发消息（通过 SDK Im.Message.Create，receive_id_type=open_id）
func SendMessageToUser(openID, text string) error {
	cli := getClient()
	if cli == nil {
		return fmt.Errorf("feishu client not initialized")
	}
	content := larkim.NewTextMsgBuilder().Text(text).Build()
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.ReceiveIdTypeOpenId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			MsgType(larkim.MsgTypeText).
			ReceiveId(openID).
			Content(content).
			Build()).
		Build()
	_, err := cli.Im.Message.Create(context.Background(), req)
	if err != nil {
		return fmt.Errorf("feishu send message: %w", err)
	}
	return nil
}
