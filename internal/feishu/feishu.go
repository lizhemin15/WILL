package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// contentJSONString 飞书要求 content 为「JSON 字符串」而非对象，否则报 230001
func contentJSONString(text string) string {
	b, _ := json.Marshal(map[string]string{"text": text})
	return string(b)
}

var (
	globalClient    *lark.Client
	globalAppID     string
	globalAppSecret string
	clientMu        sync.RWMutex
)

// TestCredentials 校验飞书 App ID / Secret 是否可用（拉取 tenant token）
func TestCredentials(appID, appSecret string) error {
	if appID == "" || appSecret == "" {
		return fmt.Errorf("app_id 或 app_secret 为空")
	}
	cli := lark.NewClient(appID, appSecret)
	ctx := context.Background()
	req := &larkcore.SelfBuiltTenantAccessTokenReq{
		AppID:     appID,
		AppSecret: appSecret,
	}
	_, err := cli.GetTenantAccessTokenBySelfBuiltApp(ctx, req)
	return err
}

// InitClient 使用 appID、appSecret 初始化全局飞书 SDK 客户端，回复与发消息均通过该客户端
func InitClient(appID, appSecret string) {
	clientMu.Lock()
	defer clientMu.Unlock()
	globalClient = lark.NewClient(appID, appSecret)
	globalAppID = appID
	globalAppSecret = appSecret
}

// GetCredentials 返回当前已初始化的 appID 和 appSecret
func GetCredentials() (appID, appSecret string) {
	clientMu.RLock()
	defer clientMu.RUnlock()
	return globalAppID, globalAppSecret
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
	content := contentJSONString(text)
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			Content(content).
			MsgType(larkim.MsgTypeText).
			Build()).
		Build()
	resp, err := cli.Im.Message.Reply(context.Background(), req)
	if err != nil {
		return fmt.Errorf("feishu reply: %w", err)
	}
	if resp != nil && resp.Code != 0 {
		return fmt.Errorf("feishu reply code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

// InitAndListen 初始化飞书客户端并阻塞监听消息。
// mode: "ws"（长连接，默认）或 "webhook"（暂未实现）。
// onMessage(openID, text, messageID) 负责处理消息并自行发送回复。
func InitAndListen(appID, appSecret, mode string, onMessage func(openID, text, messageID string)) error {
	InitClient(appID, appSecret)
	if mode == "webhook" {
		return fmt.Errorf("webhook 模式尚未实现，请将 FEISHU_SUBSCRIBE_MODE 设为 ws")
	}
	StartWSClient(appID, appSecret, func(openID, messageID, content string) (string, bool) {
		onMessage(openID, content, messageID)
		return "", false
	})
	return nil
}

// SendMessageToUser 主动给用户发消息（通过 SDK Im.Message.Create，receive_id_type=open_id）
func SendMessageToUser(openID, text string) error {
	cli := getClient()
	if cli == nil {
		return fmt.Errorf("feishu client not initialized")
	}
	content := contentJSONString(text)
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.ReceiveIdTypeOpenId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			MsgType(larkim.MsgTypeText).
			ReceiveId(openID).
			Content(content).
			Build()).
		Build()
	resp, err := cli.Im.Message.Create(context.Background(), req)
	if err != nil {
		return fmt.Errorf("feishu send message: %w", err)
	}
	// 飞书 API 可能 HTTP 200 但 body 里 code!=0（如 230013 机器人对该用户无可用性）
	if resp != nil && resp.Code != 0 {
		return fmt.Errorf("feishu code=%d msg=%s（若为 230013 请在开放平台「应用发布」→「可用性」里把测试用户加入并发布）", resp.Code, resp.Msg)
	}
	return nil
}
