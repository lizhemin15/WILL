package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yourusername/will/internal/bot"
	"github.com/yourusername/will/internal/config"
	"github.com/yourusername/will/internal/exec"
	"github.com/yourusername/will/internal/feishu"
	"github.com/yourusername/will/internal/internalapi"
	"github.com/yourusername/will/internal/llm"
	"github.com/yourusername/will/internal/orchestrator"
	"github.com/yourusername/will/internal/peer"
	"github.com/yourusername/will/internal/setup"
	"github.com/yourusername/will/internal/skill"
	"github.com/yourusername/will/internal/store"
	"github.com/yourusername/will/internal/updater"
)

// globalHub 主节点模式下持有 WebSocket hub（worker 模式为 nil）
var globalHub *peer.Hub

// ── 消息去重（OpenClaw 风格：防止飞书重推导致重复处理）────────────────────────
var (
	seenMsgIDs   = make(map[string]struct{})
	seenMsgQueue []string
	seenMsgMu    sync.Mutex
)

const maxSeenMsgs = 200

// markSeen 首次见到返回 true；已处理过返回 false
func markSeen(id string) bool {
	if id == "" {
		return true
	}
	seenMsgMu.Lock()
	defer seenMsgMu.Unlock()
	if _, exists := seenMsgIDs[id]; exists {
		return false
	}
	seenMsgIDs[id] = struct{}{}
	seenMsgQueue = append(seenMsgQueue, id)
	if len(seenMsgQueue) > maxSeenMsgs {
		delete(seenMsgIDs, seenMsgQueue[0])
		seenMsgQueue = seenMsgQueue[1:]
	}
	return true
}

// Version 由构建时 -ldflags "-X main.Version=vX.Y.Z" 注入
var Version = "dev"

func main() {
	if len(os.Args) >= 2 && (os.Args[1] == "skill" || os.Args[1] == "skills") {
		runSkillCLI()
		return
	}

	dbPath := os.Getenv("WILL_DB") // 可选，如 /opt/will/will.db；空则用 will.db
	s, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer s.Close()

	cfg := setup.RunStartup(s)
	if cfg == nil {
		cfg = config.LoadFromStore(s)
	}

	// 按模式启动对应服务
	switch cfg.Mode {
	case config.ModeWorker:
		// 从节点：通过 WebSocket 连接主节点，无需暴露端口
		wc := &peer.WorkerClient{
			MainURL: cfg.MainURL,
			Token:   cfg.InternalToken,
			Name:    cfg.WorkerName,
			Store:   s,
		}
		go wc.Start(context.Background())
		log.Printf("[worker] 从节点「%s」已启动，连接主节点 %s", cfg.WorkerName, cfg.MainURL)
		select {}
	case config.ModeMain:
		// 主节点：创建 hub，启动 HTTP 服务（pair + WebSocket）
		globalHub = peer.NewHub(s)
		go startMainHTTP(s, cfg)
	}

	// 尽早初始化飞书客户端，确保更新后通知能正常发出
	if cfg.FeishuAppID != "" && cfg.FeishuAppSecret != "" {
		feishu.InitClient(cfg.FeishuAppID, cfg.FeishuAppSecret)
	}

	// 检查更新后通知（必须在 InitClient 之后）
	if notifyID, ok := s.GetConfig(store.ConfigKeyPostUpdateNotifyOpenID); ok && notifyID != "" {
		_ = s.SetConfig(store.ConfigKeyPostUpdateNotifyOpenID, "")
		ver := strings.TrimPrefix(Version, "v")
		notes := updater.ReleaseNotes(ver)
		msg := "WILL 已更新到 v" + ver + "。"
		if notes != "" {
			msg += "\n\n更新内容：\n" + notes
		}
		if err := feishu.SendMessageToUser(notifyID, msg); err != nil {
			log.Printf("[updater] 发送更新通知失败: %v", err)
		}
	}

	// 启动定时任务 goroutine
	go runScheduledTasks(s)

	if cfg.FeishuAppID != "" && cfg.FeishuAppSecret != "" {
		if err := feishu.InitAndListen(cfg.FeishuAppID, cfg.FeishuAppSecret, cfg.FeishuSubscribeMode, func(openID, text, messageID string) {
			if !markSeen(messageID) {
				log.Printf("[feishu] 跳过重复消息 %s", messageID)
				return
			}
			reply, shouldSend := processFeishuMessage(s, cfg, openID, text, messageID)
			if shouldSend && reply != "" {
				if err := feishu.SendMessageToUser(openID, reply); err != nil {
					log.Printf("[feishu] 发消息失败: %v", err)
				} else {
					log.Printf("[feishu] 已发消息给 open_id=%q", openID)
				}
			}
		}); err != nil {
			log.Fatalf("飞书初始化失败: %v", err)
		}
	} else {
		log.Println("未配置飞书，仅后台运行。")
		select {} // 保持进程存活
	}
}

func processFeishuMessage(s *store.Store, cfg *config.Config, openID, text, messageID string) (reply string, sendReply bool) {
	// 首次用户：自动授权
	if len(cfg.FeishuAllowed) == 0 {
		_ = s.AddAllowedOpenID(openID)
		cfg = config.LoadFromStore(s)
	}

	if len(cfg.FeishuAllowed) > 0 && !feishu.IsAllowed(openID, cfg.FeishuAllowed) {
		return "未授权用户，忽略。", true
	}

	// 更新回复拦截
	if tryHandleUpdateReply(s, cfg, openID, text, messageID) {
		return "", false
	}

	// 待确认配置变更拦截
	if rpl, ok := handlePendingConfigConfirm(s, openID, strings.TrimSpace(text)); ok {
		reply, sendReply = rpl, true
		goto done
	}

	// 待确认 Skill 敏感操作拦截
	if rpl, ok := handlePendingSkillConfirm(s, openID, strings.TrimSpace(text)); ok {
		reply, sendReply = rpl, true
		goto done
	}

	// /workers 列出从节点
	if strings.ToLower(strings.TrimSpace(text)) == "/workers" {
		if globalHub != nil {
			reply, sendReply = globalHub.WorkersText(), true
		} else {
			reply, sendReply = "当前为独立或从节点模式，无从节点管理功能。", true
		}
		goto done
	}

	// /compact 需要 LLM，在 main 层处理
	if strings.ToLower(strings.TrimSpace(text)) == "/compact" {
		reply, sendReply = compactConversation(s, cfg, openID), true
		goto done
	}

	// / 命令
	if rpl, ok := bot.HandleCommand(text, openID, s, cfg); ok {
		reply, sendReply = rpl, true
		goto done
	}

	// 自动压缩：对话超过 40 条时先摘要，避免上下文窗口过大
	if s != nil && s.ConversationCount(openID) >= 40 {
		if summary, err := llm.CallChat(cfg,
			"请将以下对话历史压缩为简洁摘要（200字以内），保留关键决定、任务和用户偏好：",
			buildConvText(s, openID, 50)); err == nil && summary != "" {
			_ = s.ClearConversation(openID)
			_ = s.AppendConversation(openID, "system", "[自动压缩摘要] "+summary)
			log.Printf("[compact] 已自动压缩 open_id=%s 对话历史", openID)
		}
	}

	reply, sendReply = runWithLLM(s, cfg, openID, text), true

done:
	if sendReply && s != nil && openID != "" && reply != "" {
		_ = s.AppendConversation(openID, "user", text)
		_ = s.AppendConversation(openID, "assistant", reply)
	}
	return reply, sendReply
}

// runWithLLM 调用 LLM（Function Calling），executor 负责执行工具
func runWithLLM(s *store.Store, cfg *config.Config, openID, userMessage string) string {
	loc := loadLoc(cfg)
	executor := func(name string, argsJSON []byte) string {
		return executeTool(s, cfg, openID, loc, name, argsJSON)
	}
	reply, err := llm.Call(cfg, "user:"+openID, userMessage, s, executor)
	if err != nil {
		return "LLM 调用失败 — " + err.Error()
	}
	if reply == "" {
		return "已处理。"
	}
	return reply
}

// executeTool 根据工具名分发执行，返回给 LLM 的结果字符串
func executeTool(s *store.Store, cfg *config.Config, openID string, loc *time.Location, name string, argsJSON []byte) string {
	switch name {

	case "todo_list":
		list, err := s.ListTodos(openID)
		if err != nil {
			return "获取待办失败: " + err.Error()
		}
		return formatTodoList(list)

	case "todo_add":
		var p struct{ Title string `json:"title"` }
		_ = json.Unmarshal(argsJSON, &p)
		title := strings.TrimSpace(p.Title)
		if title == "" {
			return "待办内容不能为空。"
		}
		id, err := s.AddTodo(openID, title)
		if err != nil {
			return "添加待办失败: " + err.Error()
		}
		return fmt.Sprintf("已添加待办：%s（编号: %d）", title, id)

	case "todo_update":
		var p struct {
			Index    string `json:"index"`
			NewTitle string `json:"new_title"`
		}
		_ = json.Unmarshal(argsJSON, &p)
		newTitle := strings.TrimSpace(p.NewTitle)
		if newTitle == "" {
			return "新标题不能为空。"
		}
		idx, err := strconv.Atoi(strings.TrimSpace(p.Index))
		if err != nil || idx < 1 {
			return "序号无效，请传入 1 开始的数字。"
		}
		list, err := s.ListTodos(openID)
		if err != nil {
			return "获取待办失败: " + err.Error()
		}
		if idx > len(list) {
			return fmt.Sprintf("序号 %d 超出范围，当前共 %d 条。", idx, len(list))
		}
		if err := s.UpdateTodoTitle(list[idx-1].ID, openID, newTitle); err != nil {
			return "修改失败: " + err.Error()
		}
		return fmt.Sprintf("已将待办 [%d] 修改为：%s", idx, newTitle)

	case "todo_done", "todo_delete":
		var p struct{ Indices string `json:"indices"` }
		_ = json.Unmarshal(argsJSON, &p)
		list, err := s.ListTodos(openID)
		if err != nil {
			return "获取待办失败: " + err.Error()
		}
		ids, indices, errMsg := resolveTodoIDs(p.Indices, list)
		if errMsg != "" {
			return errMsg
		}
		if name == "todo_done" {
			for _, tid := range ids {
				if ferr := s.CompleteTodo(tid, openID); ferr != nil {
					log.Printf("[todo] 完成待办 %d 失败: %v", tid, ferr)
				}
			}
			return fmt.Sprintf("已完成待办 %s。", formatIndices(indices))
		}
		for _, tid := range ids {
			if ferr := s.DeleteTodo(tid, openID); ferr != nil {
				log.Printf("[todo] 删除待办 %d 失败: %v", tid, ferr)
			}
		}
		return fmt.Sprintf("已删除待办 %s。", formatIndices(indices))

	case "version_check":
		return updater.VersionCheckReply(Version)

	case "schedule_list":
		list, err := s.ListUserScheduledTasks(openID)
		if err != nil {
			return "读取定时任务失败: " + err.Error()
		}
		return formatScheduleList(list, loc)

	case "schedule_add":
		var p struct {
			Instruction string `json:"instruction"`
			CronExpr    string `json:"cron_expr"`
		}
		_ = json.Unmarshal(argsJSON, &p)
		p.Instruction = strings.TrimSpace(p.Instruction)
		p.CronExpr = strings.TrimSpace(p.CronExpr)
		if p.Instruction == "" {
			return "任务内容不能为空。"
		}
		if p.CronExpr == "" {
			return "cron 表达式不能为空，如 \"0 9 * * *\"（每天9点）。"
		}
		nextRun, err := store.NextCronRun(p.CronExpr, time.Now().In(loc))
		if err != nil {
			return "cron 表达式无效: " + err.Error()
		}
		id, err := s.AddUserScheduledTask(openID, p.Instruction, p.CronExpr, nextRun.Unix())
		if err != nil {
			return "添加失败: " + err.Error()
		}
		desc := store.CronDescription(p.CronExpr)
		return fmt.Sprintf("已添加定时任务 [%d]，%s，首次执行 %s。", id, desc, nextRun.In(loc).Format("2006-01-02 15:04"))

	case "schedule_delete":
		var p struct{ ID string `json:"id"` }
		_ = json.Unmarshal(argsJSON, &p)
		id, err := strconv.ParseInt(strings.TrimSpace(p.ID), 10, 64)
		if err != nil {
			return "任务编号需为数字。"
		}
		ok, err := s.DeleteUserScheduledTask(id, openID)
		if err != nil {
			return "删除失败: " + err.Error()
		}
		if !ok {
			return "未找到该任务或无权操作。"
		}
		return fmt.Sprintf("已删除定时任务 [%d]。", id)

	case "schedule_update":
		var p struct {
			ID          string `json:"id"`
			Instruction string `json:"instruction"`
			CronExpr    string `json:"cron_expr"`
		}
		_ = json.Unmarshal(argsJSON, &p)
		id, err := strconv.ParseInt(strings.TrimSpace(p.ID), 10, 64)
		if err != nil {
			return "任务编号需为数字。"
		}
		task, err := s.GetUserScheduledTaskByID(id, openID)
		if err != nil || task == nil {
			return fmt.Sprintf("未找到任务 [%s]。", p.ID)
		}
		instruction := task.Instruction
		cronExpr := task.CronExpr
		if strings.TrimSpace(p.Instruction) != "" {
			instruction = strings.TrimSpace(p.Instruction)
		}
		if strings.TrimSpace(p.CronExpr) != "" {
			cronExpr = strings.TrimSpace(p.CronExpr)
		}
		nextRun, err := store.NextCronRun(cronExpr, time.Now().In(loc))
		if err != nil {
			return "cron 表达式无效: " + err.Error()
		}
		ok, err := s.UpdateUserScheduledTask(id, openID, instruction, cronExpr, nextRun.Unix())
		if err != nil || !ok {
			return "更新失败。"
		}
		desc := store.CronDescription(cronExpr)
		return fmt.Sprintf("已更新定时任务 [%d]，%s，下次 %s。", id, desc, nextRun.In(loc).Format("2006-01-02 15:04"))

	case "schedule_run_now":
		var p struct{ ID string `json:"id"` }
		_ = json.Unmarshal(argsJSON, &p)
		id, err := strconv.ParseInt(strings.TrimSpace(p.ID), 10, 64)
		if err != nil {
			return "任务编号需为数字。"
		}
		task, err := s.GetUserScheduledTaskByID(id, openID)
		if err != nil || task == nil {
			return fmt.Sprintf("未找到任务 [%s]。", p.ID)
		}
		result, err := llm.CallForInstruction(cfg, "user:"+openID, task.Instruction, s)
		if err != nil {
			return "执行失败: " + err.Error()
		}
		return result

	case "memory_set":
		var p struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		_ = json.Unmarshal(argsJSON, &p)
		p.Key = strings.TrimSpace(p.Key)
		p.Value = strings.TrimSpace(p.Value)
		if p.Key == "" {
			return "键名不能为空。"
		}
		_ = s.SetMemory("user:"+openID, p.Key, p.Value)
		return "已记住：" + p.Key + " = " + p.Value

	case "config_change":
		var p struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		_ = json.Unmarshal(argsJSON, &p)
		p.Key = strings.TrimSpace(strings.ToLower(p.Key))
		p.Value = strings.TrimSpace(p.Value)
		storeKey, ok := llm.AllowedConfigKeys[p.Key]
		if !ok {
			return "不支持修改该配置项: " + p.Key
		}
		// 写入待确认，等用户回复「确认」
		pending := map[string]string{storeKey: p.Value}
		pendingJSON, _ := json.Marshal(pending)
		_ = s.SetMemory("user:"+openID, llm.PendingConfigKey, string(pendingJSON))
		return fmt.Sprintf("将修改配置 %s，请回复「确认」生效或「取消」忽略。", p.Key)

	case "shell_exec":
		var p struct{ Command string `json:"command"` }
		_ = json.Unmarshal(argsJSON, &p)
		return runTask(s, cfg, strings.TrimSpace(p.Command))

	case "worker_list":
		if globalHub == nil {
			return "当前不是主节点模式，无从节点管理。"
		}
		return globalHub.WorkersText()

	case "worker_exec":
		var p struct {
			WorkerName string `json:"worker_name"`
			Command    string `json:"command"`
		}
		_ = json.Unmarshal(argsJSON, &p)
		if globalHub == nil {
			return "当前不是主节点模式。"
		}
		if p.WorkerName == "" || p.Command == "" {
			return "请指定从节点名称和要执行的命令。"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		res, err := globalHub.ExecNamed(ctx, p.WorkerName, p.Command, "", 300)
		if err != nil {
			return fmt.Sprintf("从节点「%s」执行失败: %v", p.WorkerName, err)
		}
		return peer.FormatResult(res)

	case "worker_update":
		var p struct {
			WorkerName string `json:"worker_name"`
		}
		_ = json.Unmarshal(argsJSON, &p)
		if globalHub == nil {
			return "当前不是主节点模式。"
		}
		if p.WorkerName == "" {
			return "请指定要升级的从节点名称。"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := globalHub.TriggerUpdate(ctx, p.WorkerName); err != nil {
			return fmt.Sprintf("触发从节点「%s」升级失败: %v", p.WorkerName, err)
		}
		return fmt.Sprintf("已向从节点「%s」发送升级指令，升级完成后将自动重连。", p.WorkerName)

	case "skill_search":
		var p struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(argsJSON, &p)
		query := strings.TrimSpace(p.Query)
		if query == "" {
			return "请提供搜索关键词，如：pdf、vue、文档、邮件。"
		}
		r, err := skill.Search("", query)
		if err != nil {
			return "搜索失败：" + err.Error()
		}
		return skill.FormatSearchResult(r)

	case "skill_list_local":
		return bot.SkillRun([]string{"list"})

	case "skill_list_remote":
		return bot.SkillRun([]string{"list", "--remote"})

	case "skill_install":
		var p struct {
			NameOrURL string `json:"name_or_url"`
		}
		_ = json.Unmarshal(argsJSON, &p)
		nameOrURL := strings.TrimSpace(p.NameOrURL)
		if nameOrURL == "" {
			return "请提供要安装的 Skill 名称或下载链接。"
		}
		pending, _ := json.Marshal(map[string]string{"action": "install", "name_or_url": nameOrURL})
		_ = s.SetMemory("user:"+openID, llm.PendingSkillKey, string(pending))
		return fmt.Sprintf("将安装 Skill：%s。请回复「确认」执行，或「取消」放弃。", nameOrURL)

	case "skill_prepare":
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(argsJSON, &p)
		name := strings.TrimSpace(p.Name)
		if name == "" {
			return "请提供要准备依赖的 Skill 名称。"
		}
		pending, _ := json.Marshal(map[string]string{"action": "prepare", "name": name})
		_ = s.SetMemory("user:"+openID, llm.PendingSkillKey, string(pending))
		return fmt.Sprintf("将为 Skill「%s」安装依赖（如 brew）。请回复「确认」执行，或「取消」放弃。", name)

	case "skill_update":
		pending, _ := json.Marshal(map[string]string{"action": "update"})
		_ = s.SetMemory("user:"+openID, llm.PendingSkillKey, string(pending))
		return "将从注册表批量更新已安装的 Skill。请回复「确认」执行，或「取消」放弃。"

	default:
		return fmt.Sprintf("未知工具: %s", name)
	}
}

// ── Update handling ───────────────────────────────────────────────────────────

func tryHandleUpdateReply(s *store.Store, cfg *config.Config, openID, text, messageID string) bool {
	intent, err := llm.ParseUpdateReply(cfg, text)
	if err != nil || intent.Action == "" {
		return false
	}
	notifyID, _ := s.GetConfig(store.ConfigKeyUpdateNotifyOpenID)
	if notifyID != "" && notifyID != openID {
		return false
	}
	latestVer, _ := s.GetConfig(store.ConfigKeyLatestVersion)
	switch intent.Action {
	case "now":
		_, assetURL, err := updater.CheckLatest()
		if err != nil || assetURL == "" {
			_ = feishu.SendMessageToUser(openID, "获取更新失败: "+err.Error())
			return true
		}
		_ = feishu.SendMessageToUser(openID, "正在下载更新，完成后将自动重启…")
		s.SetConfig(store.ConfigKeyPostUpdateNotifyOpenID, openID)
		if err := updater.DownloadAndApply(assetURL); err != nil {
			s.SetConfig(store.ConfigKeyPostUpdateNotifyOpenID, "")
			_ = feishu.SendMessageToUser(openID, "更新失败: "+err.Error())
		}
	case "skip":
		_ = s.SetConfig(store.ConfigKeyUpdateNotifyOpenID, "")
		_ = feishu.SendMessageToUser(openID, "已跳过本次更新。")
	case "later":
		h := intent.RemindHours
		if h <= 0 {
			h = 24
		}
		payload, _ := json.Marshal(map[string]string{"version": latestVer, "open_id": openID})
		_, _ = s.AddScheduledTask(store.KindRemindUpdate, string(payload), time.Now().Add(time.Duration(h)*time.Hour).Unix())
		_ = feishu.SendMessageToUser(openID, fmt.Sprintf("好的，%d小时后再提醒你。", h))
	}
	return true
}

func handlePendingConfigConfirm(s *store.Store, openID, text string) (reply string, handled bool) {
	scope := "user:" + openID
	pendingJSON, ok := s.GetMemory(scope, llm.PendingConfigKey)
	if !ok || pendingJSON == "" {
		return "", false
	}
	lower := strings.ToLower(strings.TrimSpace(text))
	confirmWords := []string{"确认", "确定", "好的", "ok", "yes", "是", "同意"}
	cancelWords := []string{"取消", "不", "no", "否", "算了", "不要", "不用"}
	for _, w := range confirmWords {
		if strings.Contains(lower, w) {
			var pending map[string]string
			if err := json.Unmarshal([]byte(pendingJSON), &pending); err == nil {
				for k, v := range pending {
					_ = s.SetConfig(k, v)
				}
			}
			_ = s.DeleteMemory(scope, llm.PendingConfigKey)
			return "配置已更新，重启后完全生效。", true
		}
	}
	for _, w := range cancelWords {
		if strings.Contains(lower, w) {
			_ = s.DeleteMemory(scope, llm.PendingConfigKey)
			return "已取消配置变更。", true
		}
	}
	return "", false
}

func handlePendingSkillConfirm(s *store.Store, openID, text string) (reply string, handled bool) {
	scope := "user:" + openID
	pendingJSON, ok := s.GetMemory(scope, llm.PendingSkillKey)
	if !ok || pendingJSON == "" {
		return "", false
	}
	lower := strings.ToLower(strings.TrimSpace(text))
	confirmWords := []string{"确认", "确定", "好的", "ok", "yes", "是", "同意"}
	cancelWords := []string{"取消", "不", "no", "否", "算了", "不要", "不用"}
	for _, w := range cancelWords {
		if strings.Contains(lower, w) {
			_ = s.DeleteMemory(scope, llm.PendingSkillKey)
			return "已取消。", true
		}
	}
	for _, w := range confirmWords {
		if strings.Contains(lower, w) {
			var pending struct {
				Action    string `json:"action"`
				NameOrURL string `json:"name_or_url"`
				Name      string `json:"name"`
			}
			_ = json.Unmarshal([]byte(pendingJSON), &pending)
			_ = s.DeleteMemory(scope, llm.PendingSkillKey)
			var args []string
			switch pending.Action {
			case "install":
				args = []string{"install", strings.TrimSpace(pending.NameOrURL)}
			case "prepare":
				args = []string{"prepare", strings.TrimSpace(pending.Name)}
			case "update":
				args = []string{"update"}
			default:
				return "未知操作，已清除。", true
			}
			reply := bot.SkillRun(args)
			if pending.Action == "install" && strings.TrimSpace(pending.NameOrURL) != "" {
				desc, body := skill.GetBodyByName("", strings.TrimSpace(pending.NameOrURL))
				if body != "" {
					reply += "\n\n【技能说明】\n" + desc + "\n\n" + body
					reply += "\n\n请使用 shell_exec 执行上述技能说明中的命令完成用户任务。"
				}
			}
			return reply, true
		}
	}
	return "", false
}

// ── Scheduled task runner ─────────────────────────────────────────────────────

func runScheduledTasks(s *store.Store) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for range tick.C {
		now := time.Now().Unix()
		tasks, err := s.ListScheduledTasksDue(now)
		if err != nil || len(tasks) == 0 {
			continue
		}
		cfg := config.LoadFromStore(s)
		loc := loadLoc(cfg)
		for _, t := range tasks {
			_ = s.DeleteScheduledTask(t.ID)
			switch t.Kind {
			case store.KindDoVersionCheck:
				doVersionCheck(s)
			case store.KindRemindUpdate:
				var payload struct {
					Version string `json:"version"`
					OpenID  string `json:"open_id"`
				}
				_ = json.Unmarshal([]byte(t.Payload), &payload)
				if payload.OpenID != "" && cfg.FeishuAppID != "" {
					msg := "WILL 提醒：新版本 v" + payload.Version + " 仍未更新，是否现在更新？回复「立即更新」或「稍后」或「不更新」。"
					_ = feishu.SendMessageToUser(payload.OpenID, msg)
					_ = s.SetConfig(store.ConfigKeyUpdateNotifyOpenID, payload.OpenID)
					_ = s.SetConfig(store.ConfigKeyLatestVersion, payload.Version)
				}
			case store.KindUserScheduled:
				var p store.UserScheduledPayload
				_ = json.Unmarshal([]byte(t.Payload), &p)
				if p.Instruction == "" || t.OpenID == "" {
					continue
				}
				reply, err := llm.CallForInstruction(cfg, "user:"+t.OpenID, p.Instruction, s)
				if err != nil {
					reply = "定时任务执行失败: " + err.Error()
				}
				if cfg.FeishuAppID != "" && cfg.FeishuAppSecret != "" {
					_ = feishu.SendMessageToUser(t.OpenID, reply)
				}
				// 用 cron 表达式计算下次执行时间
				if p.CronExpr != "" {
					nextRun, err := store.NextCronRun(p.CronExpr, time.Now().In(loc))
					if err == nil {
						_, _ = s.AddUserScheduledTask(t.OpenID, p.Instruction, p.CronExpr, nextRun.Unix())
					}
				}
			}
		}
	}
}

func doVersionCheck(s *store.Store) {
	latestVer, assetURL, err := updater.CheckLatest()
	if err != nil {
		log.Printf("[updater] 检查更新失败: %v", err)
		return
	}
	_ = s.SetConfig(store.ConfigKeyLatestVersion, latestVer)
	if !updater.CompareVersion(latestVer, Version) {
		return
	}
	if assetURL == "" {
		return
	}
	notifyID, _ := s.GetConfig(store.ConfigKeyUpdateNotifyOpenID)
	if notifyID == "" {
		ids := s.GetAllowedOpenIDs()
		if len(ids) > 0 {
			notifyID = ids[0]
		}
	}
	if notifyID == "" {
		return
	}
	msg := fmt.Sprintf("发现新版本 v%s，回复「立即更新」可更新，「稍后N小时」可延后提醒。", latestVer)
	_ = feishu.SendMessageToUser(notifyID, msg)
	_ = s.SetConfig(store.ConfigKeyUpdateNotifyOpenID, notifyID)
	_ = s.SetConfig(store.ConfigKeyUpdatePromptAt, strconv.FormatInt(time.Now().Unix(), 10))
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func loadLoc(cfg *config.Config) *time.Location {
	tz := "Asia/Shanghai"
	if cfg != nil && cfg.Timezone != "" {
		tz = cfg.Timezone
	}
	loc, err := time.LoadLocation(tz)
	if err != nil || loc == nil {
		loc, _ = time.LoadLocation("Asia/Shanghai")
		if loc == nil {
			loc = time.UTC
		}
	}
	return loc
}

func formatTodoList(list []store.TodoItem) string {
	if len(list) == 0 {
		return "当前无待办。"
	}
	var b strings.Builder
	b.WriteString("待办列表：\n")
	for i, t := range list {
		status := "⬜ 未完成"
		if t.Done {
			status = "✅ 已完成"
		}
		b.WriteString(fmt.Sprintf("[%d] %s (%s)\n", i+1, t.Title, status))
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatScheduleList(list []store.UserScheduledTask, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	if len(list) == 0 {
		return "当前无定时任务。"
	}
	var b strings.Builder
	b.WriteString("定时任务列表：\n")
	for _, t := range list {
		desc := store.CronDescription(t.CronExpr)
		if desc == t.CronExpr {
			desc = t.CronExpr
		}
		next := time.Unix(t.RunAt, 0).In(loc).Format("2006-01-02 15:04")
		b.WriteString(fmt.Sprintf("[%d] %s（下次 %s）— %s\n", t.ID, desc, next, t.Instruction))
	}
	b.WriteString("\n说「删除定时任务 2」或「修改定时任务 2 改为每天10点」")
	return strings.TrimRight(b.String(), "\n")
}

func resolveTodoIDs(indicesStr string, list []store.TodoItem) (ids []int64, indices []int, errMsg string) {
	if indicesStr == "" {
		return nil, nil, "请指定待办序号（1开始，多个逗号分隔，如 1 或 1,2）。"
	}
	parts := strings.FieldsFunc(indicesStr, func(r rune) bool {
		return r == ',' || r == '，' || r == ' ' || r == '、'
	})
	seen := make(map[int]bool)
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 1 || n > len(list) || seen[n] {
			continue
		}
		seen[n] = true
		ids = append(ids, list[n-1].ID)
		indices = append(indices, n)
	}
	if len(ids) == 0 {
		return nil, nil, fmt.Sprintf("序号需在 1 到 %d 之间。", len(list))
	}
	return ids, indices, ""
}

func formatIndices(indices []int) string {
	if len(indices) == 0 {
		return ""
	}
	parts := make([]string, len(indices))
	for i, n := range indices {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, "、")
}

func runTask(s *store.Store, cfg *config.Config, instruction string) string {
	if instruction == "" {
		return "收到空指令。"
	}
	firstWord := instruction
	if i := strings.IndexAny(instruction, " \t"); i > 0 {
		firstWord = instruction[:i]
	}
	if strings.ToLower(firstWord) == "git" {
		return "检查版本请说「检查新版本」，无需 git。"
	}
	// 主节点且有在线从节点时，通过 WebSocket 派发给从节点执行
	if cfg.Mode == config.ModeMain && globalHub != nil && globalHub.WorkerCount() > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		res, err := globalHub.Exec(ctx, instruction, "", 300)
		if err != nil {
			return "从节点调用失败 — " + err.Error()
		}
		return peer.FormatResult(res)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res := exec.Run(ctx, instruction, "", 5*time.Minute)
	return res.String()
}

// orchestrator 入口：复杂多步任务用规划-执行-审查，简单任务直接 runWithLLM
func runMultiAgent(s *store.Store, cfg *config.Config, openID, userMessage string) string {
	if !isMultiStepRequest(userMessage) {
		return runWithLLM(s, cfg, openID, userMessage)
	}
	runStep := func(step string) string {
		return runWithLLM(s, cfg, openID, step)
	}
	return orchestrator.Run(cfg, userMessage, runStep)
}

func isMultiStepRequest(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}
	singleOp := []string{"定时任务", "待办", "版本", "配置", "记忆", "执行"}
	for _, p := range singleOp {
		if strings.Contains(lower, p) {
			return false
		}
	}
	signals := []string{"然后", "接着", "之后", "同时", "第一步", "第二步"}
	count := 0
	for _, sig := range signals {
		if strings.Contains(lower, sig) {
			count++
			if count >= 2 {
				return true
			}
		}
	}
	return false
}

// ── /compact（参考 OpenClaw /compact，AI 摘要压缩对话历史）─────────────────────

// compactConversation 用 LLM 将对话历史压缩为摘要，清空旧记录并写入摘要条目
func compactConversation(s *store.Store, cfg *config.Config, openID string) string {
	if s == nil {
		return "未启用本地存储。"
	}
	count := s.ConversationCount(openID)
	if count < 6 {
		return fmt.Sprintf("对话记录较短（%d 条），无需压缩。", count)
	}
	convText := buildConvText(s, openID, 50)
	summary, err := llm.CallChat(cfg,
		"请将以下对话历史压缩为简洁摘要（200字以内），保留关键决定、任务目标和用户偏好：",
		convText)
	if err != nil {
		return "压缩失败: " + err.Error()
	}
	_ = s.ClearConversation(openID)
	_ = s.AppendConversation(openID, "system", "[对话摘要] "+summary)
	return fmt.Sprintf("已压缩对话历史（%d 条 → 1 条摘要）。\n摘要：%s", count, summary)
}

// buildConvText 将最近 n 条对话格式化为文本供 LLM 摘要
func buildConvText(s *store.Store, openID string, n int) string {
	history, _ := s.GetRecentConversation(openID, n)
	var b strings.Builder
	for _, m := range history {
		role := "用户"
		if m.Role == "assistant" {
			role = "助手"
		} else if m.Role == "system" {
			role = "系统"
		}
		b.WriteString(role + ": " + m.Content + "\n")
	}
	return b.String()
}

// ── HTTP 服务（main 模式） ─────────────────────────────────────────────────────

func startMainHTTP(s *store.Store, cfg *config.Config) {
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/pair", internalapi.HandlePair(s))
	if globalHub != nil {
		mux.HandleFunc("/internal/ws", globalHub.HubHandler())
	}
	addr := cfg.Bind + ":" + cfg.Port
	log.Printf("[main] HTTP 服务监听 %s（pair + WebSocket）", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("[main] HTTP 服务失败: %v", err)
	}
}
