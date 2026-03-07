package bot

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/yourusername/will/internal/config"
	"github.com/yourusername/will/internal/store"
)

// HandleTodoNaturalLanguage 处理「待办列表」「添加待办 xxx」等自然语言，返回回复及是否已处理
func HandleTodoNaturalLanguage(text string, openID string, s *store.Store) (reply string, handled bool) {
	if s == nil || openID == "" {
		return "", false
	}
	t := strings.TrimSpace(text)
	lower := strings.ToLower(t)
	if lower == "待办" || lower == "待办列表" || lower == "我的待办" || lower == "看看待办" {
		return cmdTodo([]string{"list"}, openID, s), true
	}
	if strings.HasPrefix(lower, "添加待办") || strings.HasPrefix(lower, "待办添加") {
		prefix := "添加待办"
		if strings.HasPrefix(lower, "待办添加") {
			prefix = "待办添加"
		}
		title := strings.TrimSpace(t[len(prefix):])
		if title == "" {
			return "添加待办请说明内容，如：添加待办 买牛奶", true
		}
		return cmdTodo([]string{"add", title}, openID, s), true
	}
	return "", false
}

// HandleCommand 处理 / 开头的配置与记忆命令，返回回复文案；若不是命令或未处理则返回空字符串。
func HandleCommand(text string, openID string, s *store.Store, cfg *config.Config) (reply string, handled bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", false
	}
	parts := strings.SplitN(text, " ", 4)
	cmd := strings.ToLower(strings.TrimPrefix(parts[0], "/"))
	if len(parts) < 2 {
		switch cmd {
		case "help", "":
			return cmdHelp(), true
		case "config":
			return cmdConfigGet(s, cfg), true
		case "todo":
			return cmdTodo(nil, openID, s), true
		case "status":
			return cmdStatus(openID, s, cfg), true
		case "reset", "new":
			return cmdReset(openID, s), true
		case "pair":
			return cmdPair(openID, s, cfg), true
		}
		return "用法: /help", true
	}
	args := parts[1:]
	switch cmd {
	case "allow":
		return cmdAllow(args[0], openID, s), true
	case "config":
		return cmdConfig(args, s, cfg), true
	case "memory":
		return cmdMemory(args, openID, s), true
	case "todo":
		return cmdTodo(args, openID, s), true
	case "status":
		return cmdStatus(openID, s, cfg), true
	case "reset", "new":
		return cmdReset(openID, s), true
	default:
		return "", false
	}
}

func cmdHelp() string {
	return `WILL 命令：
/status            — 会话状态（模型、对话轮数、记忆、定时任务）
/reset  /new       — 清空对话历史，开始新会话
/compact           — 压缩对话历史（由 AI 摘要，节省上下文）
/todo              — 待办列表
/todo add <内容>   — 添加待办
/todo done <id>    — 标为已完成
/todo delete <id>  — 删除待办
/memory list       — 列出记忆
/memory set <k> <v> — 记录记忆
/config get        — 查看当前配置（密钥脱敏）
/config <key> <v>  — 修改配置（llm_api_key、llm_base_url、llm_model 等）
/allow me          — 将当前用户加入授权列表
/pair              — 生成从节点配对码（10分钟有效，用于绑定新机器人）
/workers           — 列出所有已连接的从节点名称

自然语言也可：直接说「添加待办 xxx」「每天9点提醒我」「现在几点」等，AI 会直接处理。`
}

func cmdAllow(arg string, openID string, s *store.Store) string {
	if s == nil {
		return "未启用本地存储，无法修改授权。"
	}
	if strings.ToLower(strings.TrimSpace(arg)) != "me" {
		return "目前仅支持 /allow me，将你本人加入授权。"
	}
	if openID == "" {
		return "无法获取你的 open_id。"
	}
	if err := s.AddAllowedOpenID(openID); err != nil {
		return "写入失败: " + err.Error()
	}
	return "已将你加入授权列表。"
}

func cmdConfig(args []string, s *store.Store, cfg *config.Config) string {
	if s == nil {
		return "未启用本地存储。"
	}
	if len(args) >= 1 && strings.ToLower(args[0]) == "get" {
		return cmdConfigGet(s, cfg)
	}
	if len(args) < 2 {
		return "用法: /config <key> <value>"
	}
	key := strings.TrimSpace(strings.ToLower(args[0]))
	value := strings.TrimSpace(strings.Join(args[1:], " "))

	allowedKeys := map[string]string{
		"mode":               store.ConfigKeyMode,
		"internal_token":     store.ConfigKeyInternalToken,
		"worker_urls":        store.ConfigKeyWorkerURLs,
		"port":               store.ConfigKeyPort,
		"bind":               store.ConfigKeyBind,
		"feishu_app_id":      store.ConfigKeyFeishuAppID,
		"feishu_app_secret":  store.ConfigKeyFeishuAppSecret,
		"llm_api_key":       store.ConfigKeyLLMApiKey,
		"llm_base_url":       store.ConfigKeyLLMBaseURL,
		"llm_model":             store.ConfigKeyLLMModel,
		"feishu_subscribe_mode": store.ConfigKeyFeishuSubscribeMode,
	}
	configKey, ok := allowedKeys[key]
	if !ok {
		return "未知 key，可用: mode, internal_token, worker_urls, port, bind, feishu_app_id, feishu_app_secret, llm_api_key, llm_base_url, llm_model, feishu_subscribe_mode"
	}
	if err := s.SetConfig(configKey, value); err != nil {
		return "写入失败: " + err.Error()
	}
	return "已保存 " + key + "，重启或下次请求生效。"
}

func cmdConfigGet(s *store.Store, cfg *config.Config) string {
	mask := func(v string) string {
		if v == "" {
			return "(未设置)"
		}
		if len(v) <= 8 {
			return "***"
		}
		return v[:4] + "***" + v[len(v)-2:]
	}
	out := "当前配置:\n"
	out += "mode: " + string(cfg.Mode) + "\n"
	out += "port: " + cfg.Port + "\n"
	out += "bind: " + cfg.Bind + "\n"
	out += "llm_api_key: " + mask(cfg.LLMApiKey) + "\n"
	out += "llm_base_url: " + cfg.LLMBaseURL + "\n"
	out += "llm_model: " + cfg.LLMModel + "\n"
	out += "feishu_app_id: " + mask(cfg.FeishuAppID) + "\n"
	out += "feishu_app_secret: " + mask(cfg.FeishuAppSecret) + "\n"
	out += "internal_token: " + mask(cfg.InternalToken) + "\n"
	out += "allowed_open_ids: " + strings.Join(cfg.FeishuAllowed, ",") + "\n"
	out += "worker_urls: " + strings.Join(cfg.WorkerURLs, ",") + "\n"
	out += "feishu_subscribe_mode: " + cfg.FeishuSubscribeMode + "\n"
	return out
}

func cmdMemory(args []string, openID string, s *store.Store) string {
	if s == nil {
		return "未启用本地存储。"
	}
	scope := "user:" + openID
	if len(args) < 1 {
		return "用法: /memory set <key> <value> 或 /memory get <key> 或 /memory list"
	}
	sub := strings.ToLower(strings.TrimSpace(args[0]))
	switch sub {
	case "set":
		if len(args) < 3 {
			return "用法: /memory set <key> <value>"
		}
		key := strings.TrimSpace(args[1])
		value := strings.TrimSpace(strings.Join(args[2:], " "))
		if err := s.SetMemory(scope, key, value); err != nil {
			return "写入失败: " + err.Error()
		}
		return "已记录 " + key + "。"
	case "get":
		if len(args) < 2 {
			return "用法: /memory get <key>"
		}
		key := strings.TrimSpace(args[1])
		v, ok := s.GetMemory(scope, key)
		if !ok {
			return "无此 key。"
		}
		return v
	case "list":
		m, err := s.ListMemory(scope)
		if err != nil {
			return "读取失败: " + err.Error()
		}
		if len(m) == 0 {
			return "当前无记忆。"
		}
		out := "记忆:\n"
		for k, v := range m {
			if len(v) > 60 {
				v = v[:60] + "..."
			}
			out += "- " + k + ": " + v + "\n"
		}
		return out
	default:
		return "用法: /memory set|get|list ..."
	}
}

func cmdTodo(args []string, openID string, s *store.Store) string {
	if s == nil {
		return "未启用本地存储，无法使用待办。"
	}
	if openID == "" {
		return "无法获取你的 open_id。"
	}
	if len(args) == 0 || strings.ToLower(strings.TrimSpace(args[0])) == "list" {
		list, err := s.ListTodos(openID)
		if err != nil {
			return "获取待办失败: " + err.Error()
		}
		if len(list) == 0 {
			return "当前无待办。发送 /todo add <内容> 添加。"
		}
		var b strings.Builder
		b.WriteString("待办列表：\n")
		for i, t := range list {
			status := "未完成"
			if t.Done {
				status = "已完成"
			}
			b.WriteString(fmt.Sprintf("[%d] %s (%s)\n", i+1, t.Title, status))
		}
		b.WriteString("\n/todo done 1 2 或 /todo delete 1,2 可一次操作多条")
		return b.String()
	}
	sub := strings.ToLower(strings.TrimSpace(args[0]))
	switch sub {
	case "add":
		if len(args) < 2 {
			return "用法: /todo add <内容>"
		}
		title := strings.TrimSpace(strings.Join(args[1:], " "))
		if title == "" {
			return "待办内容不能为空。"
		}
		if _, err := s.AddTodo(openID, title); err != nil {
			return "添加失败: " + err.Error()
		}
		return "已添加待办：" + title
	case "done":
		if len(args) < 2 {
			return "用法: /todo done <序号> 或 /todo done 1 2 3"
		}
		list, err := s.ListTodos(openID)
		if err != nil {
			return "获取待办失败: " + err.Error()
		}
		ids, indices := resolveTodoLocalIndices(list, args[1:])
		if len(ids) == 0 {
			return "序号需为 1 到 " + strconv.Itoa(len(list)) + " 的数字，可多个如 1 2 3"
		}
		for _, id := range ids {
			if ferr := s.CompleteTodo(id, openID); ferr != nil {
				log.Printf("[todo] 完成待办 %d 失败: %v", id, ferr)
			}
		}
		return fmt.Sprintf("已将 %s 标为已完成。", formatTodoIndices(indices))
	case "delete":
		if len(args) < 2 {
			return "用法: /todo delete <序号> 或 /todo delete 1 2 3"
		}
		list, err := s.ListTodos(openID)
		if err != nil {
			return "获取待办失败: " + err.Error()
		}
		ids, indices := resolveTodoLocalIndices(list, args[1:])
		if len(ids) == 0 {
			return "序号需为 1 到 " + strconv.Itoa(len(list)) + " 的数字，可多个如 1 2 3"
		}
		for _, id := range ids {
			if ferr := s.DeleteTodo(id, openID); ferr != nil {
				log.Printf("[todo] 删除待办 %d 失败: %v", id, ferr)
			}
		}
		return fmt.Sprintf("已删除待办 %s。", formatTodoIndices(indices))
	default:
		return "用法: /todo [list|add <内容>|done 1 2|delete 1 2]"
	}
}

func resolveTodoLocalIndices(list []store.TodoItem, args []string) (ids []int64, indices []int) {
	seen := make(map[int]bool)
	for _, a := range args {
		for _, p := range strings.FieldsFunc(a, func(r rune) bool { return r == ',' || r == '，' || r == '、' }) {
			p = strings.TrimSpace(p)
			n, err := strconv.Atoi(p)
			if err != nil || n < 1 || n > len(list) || seen[n] {
				continue
			}
			seen[n] = true
			ids = append(ids, list[n-1].ID)
			indices = append(indices, n)
		}
	}
	return ids, indices
}

// cmdStatus 展示当前会话状态（参考 OpenClaw /status）
func cmdStatus(openID string, s *store.Store, cfg *config.Config) string {
	model := "gpt-4o-mini"
	if cfg != nil && cfg.LLMModel != "" {
		model = cfg.LLMModel
	}
	baseURL := "https://api.openai.com/v1"
	if cfg != nil && cfg.LLMBaseURL != "" {
		baseURL = cfg.LLMBaseURL
	}

	var convCount, todoTotal, todoPending, schedCount, memCount int
	if s != nil {
		convCount = s.ConversationCount(openID)
		if todos, err := s.ListTodos(openID); err == nil {
			todoTotal = len(todos)
			for _, t := range todos {
				if !t.Done {
					todoPending++
				}
			}
		}
		if tasks, err := s.ListUserScheduledTasks(openID); err == nil {
			schedCount = len(tasks)
		}
		if mem, err := s.ListMemory("user:" + openID); err == nil {
			for k := range mem {
				if k != "_pending_config" {
					memCount++
				}
			}
		}
	}
	return fmt.Sprintf(
		"WILL 状态\n模型: %s\n接入点: %s\n对话记录: %d 条\n待办: %d（%d 未完成）\n定时任务: %d 个\n记忆条目: %d 条",
		model, baseURL, convCount, todoTotal, todoPending, schedCount, memCount,
	)
}

// cmdReset 清空当前用户的对话历史（参考 OpenClaw /reset /new）
func cmdReset(openID string, s *store.Store) string {
	if s == nil {
		return "未启用本地存储。"
	}
	if err := s.ClearConversation(openID); err != nil {
		return "清空失败: " + err.Error()
	}
	return "对话历史已清空，开始新对话。"
}

// cmdPair 生成一次性配对码，供新从节点在部署时使用
func cmdPair(openID string, s *store.Store, cfg *config.Config) string {
	if s == nil {
		return "未启用本地存储，无法生成配对码。"
	}
	if cfg != nil && cfg.Mode == config.ModeWorker {
		return "从节点模式下不支持生成配对码，请在主节点上执行此命令。"
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "生成配对码失败: " + err.Error()
	}
	token := "WILL-" + strings.ToUpper(hex.EncodeToString(b))
	expiry := time.Now().Add(10 * time.Minute).Unix()
	if err := s.SavePairToken(token, expiry); err != nil {
		return "保存配对码失败: " + err.Error()
	}
	return fmt.Sprintf(
		"配对码：%s\n有效期：10 分钟\n\n在新机器人节点启动时选择「机器人间通信」模式，输入本机地址和此配对码即可完成绑定。",
		token,
	)
}

func formatTodoIndices(indices []int) string {
	if len(indices) == 0 {
		return ""
	}
	if len(indices) == 1 {
		return strconv.Itoa(indices[0])
	}
	return strings.Trim(strings.Replace(fmt.Sprint(indices), " ", "、", -1), "[]")
}
