package internalapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// PairWithMain 向主节点发起配对请求，不使用 Bearer 鉴权（令牌在请求体中）
func PairWithMain(ctx context.Context, mainURL, token, workerName string) (*PairResponse, error) {
	mainURL = strings.TrimRight(mainURL, "/")
	reqBody := PairRequest{Token: token, WorkerName: workerName}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mainURL+"/internal/pair", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out PairResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return &out, fmt.Errorf("主节点返回 %d: %s", resp.StatusCode, out.Error)
	}
	return &out, nil
}

// Client 主节点调用 worker 的客户端
type Client struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

func NewClient(baseURL, token string) *Client {
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}
	return &Client{
		BaseURL: baseURL,
		Token:   token,
		Client:  &http.Client{Timeout: 6 * time.Minute},
	}
}

// Exec 向该 worker 下发命令并返回结果
func (c *Client) Exec(ctx context.Context, command, workDir string, timeoutSec int) (*ExecResponse, error) {
	reqBody := ExecRequest{
		Command: command,
		WorkDir: workDir,
		Timeout: timeoutSec,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	url := c.BaseURL + "internal/exec"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+c.Token)

	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out ExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return &out, fmt.Errorf("worker returned %d: %s", resp.StatusCode, out.Error)
	}
	return &out, nil
}
