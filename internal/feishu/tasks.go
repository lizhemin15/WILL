package feishu

import (
	"context"
	"fmt"

	larktask "github.com/larksuite/oapi-sdk-go/v3/service/task/v1"
)

// TaskInfo 代表一条飞书任务的摘要信息
type TaskInfo struct {
	ID    string
	Title string
	Done  bool
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

// UpdateTask 修改飞书任务的标题
func UpdateTask(taskID, newTitle string) error {
	cli := getClient()
	if cli == nil {
		return fmt.Errorf("feishu client not initialized")
	}
	req := larktask.NewPatchTaskReqBuilder().
		TaskId(taskID).
		UpdateFields([]string{"summary"}).
		Task(larktask.NewTaskBuilder().
			Summary(newTitle).
			Build()).
		Build()
	resp, err := cli.Task.V1.Task.Patch(context.Background(), req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("code=%d msg=%s", resp.Code, resp.Msg)
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
