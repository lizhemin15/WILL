package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larktask "github.com/larksuite/oapi-sdk-go/v3/service/task/v1"
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

// TaskInfo 代表一条飞书任务的摘要信息
type TaskInfo struct {
	ID    string
	Title string
	Done  bool
}

// ListTasks 获取所有飞书任务（分页拉取，最多 500 条）
func ListTasks() ([]TaskInfo, error) {
	cli := getClient()
	if cli == nil {
		return nil, fmt.Errorf("feishu client not initialized")
	}
	var result []TaskInfo
	var pageToken string
	for {
		builder := larktask.NewListTaskReqBuilder().
			UserIdType("open_id").
			PageSize(100)
		if pageToken != "" {
			builder = builder.PageToken(pageToken)
		}
		resp, err := cli.Task.V1.Task.List(context.Background(), builder.Build())
		if err != nil {
			return nil, fmt.Errorf("拉取飞书任务列表失败: %w", err)
		}
		if !resp.Success() {
			return nil, fmt.Errorf("code=%d msg=%s", resp.Code, resp.Msg)
		}
		for _, t := range resp.Data.Items {
			if t.Id == nil || t.Summary == nil {
				continue
			}
			info := TaskInfo{ID: *t.Id, Title: *t.Summary}
			if t.CompleteTime != nil && *t.CompleteTime != "" && *t.CompleteTime != "0" {
				info.Done = true
			}
			result = append(result, info)
		}
		if resp.Data.HasMore == nil || !*resp.Data.HasMore || resp.Data.PageToken == nil || len(result) >= 500 {
			break
		}
		pageToken = *resp.Data.PageToken
	}
	return result, nil
}

// CreateTask 在飞书任务中心创建一条任务，将 openID 用户设为执行者，返回飞书任务 ID
func CreateTask(openID, title string) (string, error) {
	cli := getClient()
	if cli == nil {
		return "", fmt.Errorf("feishu client not initialized")
	}
	req := larktask.NewCreateTaskReqBuilder().
		UserIdType("open_id").
		Task(larktask.NewTaskBuilder().
			Summary(title).
			Origin(larktask.NewOriginBuilder().
				PlatformI18nName(`{"zh_cn":"WILL 助手","en_us":"WILL"}`).
				Build()).
			CollaboratorIds([]string{openID}).
			Build()).
		Build()
	resp, err := cli.Task.V1.Task.Create(context.Background(), req)
	if err != nil {
		return "", fmt.Errorf("创建飞书任务失败: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("创建飞书任务失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.Task == nil || resp.Data.Task.Id == nil {
		return "", fmt.Errorf("创建飞书任务返回数据为空")
	}
	return *resp.Data.Task.Id, nil
}

// GetTask 获取单条飞书任务的标题与完成状态
func GetTask(taskID string) (*TaskInfo, error) {
	cli := getClient()
	if cli == nil {
		return nil, fmt.Errorf("feishu client not initialized")
	}
	req := larktask.NewGetTaskReqBuilder().
		TaskId(taskID).
		Build()
	resp, err := cli.Task.V1.Task.Get(context.Background(), req)
	if err != nil {
		return nil, err
	}
	if !resp.Success() {
		return nil, fmt.Errorf("code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.Task == nil {
		return nil, fmt.Errorf("任务不存在")
	}
	t := resp.Data.Task
	info := &TaskInfo{ID: taskID}
	if t.Summary != nil {
		info.Title = *t.Summary
	}
	if t.CompleteTime != nil && *t.CompleteTime != "" && *t.CompleteTime != "0" {
		info.Done = true
	}
	return info, nil
}

// CompleteTask 将飞书任务标记为已完成
func CompleteTask(taskID string) error {
	cli := getClient()
	if cli == nil {
		return fmt.Errorf("feishu client not initialized")
	}
	req := larktask.NewCompleteTaskReqBuilder().
		TaskId(taskID).
		Build()
	resp, err := cli.Task.V1.Task.Complete(context.Background(), req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

// UpdateTask 修改飞书任务的标题（通过原生 HTTP PATCH，SDK builder 不暴露 update_fields）
func UpdateTask(taskID, newTitle string) error {
	appID, appSecret := GetCredentials()
	if appID == "" {
		return fmt.Errorf("feishu client not initialized")
	}

	// 获取 tenant_access_token
	cli := getClient()
	tokenResp, err := cli.GetTenantAccessTokenBySelfBuiltApp(context.Background(), &larkcore.SelfBuiltTenantAccessTokenReq{
		AppID:     appID,
		AppSecret: appSecret,
	})
	if err != nil || tokenResp == nil {
		return fmt.Errorf("获取 access token 失败: %w", err)
	}

	body, _ := json.Marshal(map[string]interface{}{
		"task":          map[string]string{"summary": newTitle},
		"update_fields": []string{"summary"},
	})
	req, _ := http.NewRequest(http.MethodPatch,
		"https://open.feishu.cn/open-apis/task/v1/tasks/"+taskID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tokenResp.TenantAccessToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP 请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("响应解析失败: %w", err)
	}
	if result.Code != 0 {
		return fmt.Errorf("code=%d msg=%s", result.Code, result.Msg)
	}
	return nil
}

// DeleteTask 删除飞书任务
func DeleteTask(taskID string) error {
	cli := getClient()
	if cli == nil {
		return fmt.Errorf("feishu client not initialized")
	}
	req := larktask.NewDeleteTaskReqBuilder().
		TaskId(taskID).
		Build()
	resp, err := cli.Task.V1.Task.Delete(context.Background(), req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}
