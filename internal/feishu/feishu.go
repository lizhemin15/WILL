package feishu

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	authURL   = "https://open.feishu.cn/open_apis/auth/v3/tenant_access_token/internal"
	replyFmt  = "https://open.feishu.cn/open_apis/im/v1/messages/%s/reply"
	tokenTTL  = 7000 * time.Second
	userAgent = "WILL/1.0"
)

type tokenHolder struct {
	mu    sync.Mutex
	token string
	exp   time.Time
}

var globalToken tokenHolder

func getTenantAccessToken(appID, appSecret string) (string, error) {
	globalToken.mu.Lock()
	defer globalToken.mu.Unlock()
	if globalToken.token != "" && time.Now().Before(globalToken.exp) {
		return globalToken.token, nil
	}

	body, _ := json.Marshal(map[string]string{
		"app_id":     appID,
		"app_secret": appSecret,
	})
	req, err := http.NewRequest(http.MethodPost, authURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", userAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var out struct {
		Code  int    `json:"code"`
		Msg   string `json:"msg"`
		Token string `json:"tenant_access_token"`
		Expire int   `json:"expire"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", fmt.Errorf("feishu auth: code=%d msg=%s", out.Code, out.Msg)
	}

	globalToken.token = out.Token
	expSec := out.Expire
	if expSec <= 0 {
		expSec = 7200
	}
	globalToken.exp = time.Now().Add(time.Duration(expSec) * time.Second)
	return globalToken.token, nil
}

func ReplyMessage(appID, appSecret, messageID, text string) error {
	token, err := getTenantAccessToken(appID, appSecret)
	if err != nil {
		return err
	}

	payload := map[string]string{
		"msg_type": "text",
		"content":  `{"text":"` + escapeJSONString(text) + `"}`,
	}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf(replyFmt, messageID)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", userAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if out.Code != 0 {
		return fmt.Errorf("feishu reply: code=%d msg=%s", out.Code, out.Msg)
	}
	return nil
}

func escapeJSONString(s string) string {
	var b []byte
	for _, r := range s {
		switch r {
		case '"', '\\':
			b = append(b, '\\', byte(r))
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default:
			if r < 32 {
				continue
			}
			b = append(b, string(r)...)
		}
	}
	return string(b)
}
