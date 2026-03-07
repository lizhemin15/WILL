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
	"time"

	"github.com/yourusername/will/internal/bot"
	"github.com/yourusername/will/internal/config"
	"github.com/yourusername/will/internal/exec"
	"github.com/yourusername/will/internal/feishu"
	"github.com/yourusername/will/internal/internalapi"
	"github.com/yourusername/will/internal/llm"
	"github.com/yourusername/will/internal/setup"
	"github.com/yourusername/will/internal/store"
	"github.com/yourusername/will/internal/updater"
)

// Version 由构建时注入：-ldflags "-X main.Version=v0.0.3"
var Version = "dev"

func main() {
	var s *store.Store
	cfg := config.Load()

	if cfg.Mode != config.ModeWorker {
		if st, err := store.Open(""); err != nil {
			log.Printf("WILL: 未使用本地数据库 (%v)，仅从环境变量加载配置。", err)
		} else {
			s = st
			defer s.Close()
			cfg = config.LoadFromStore(s)
			// 启动时检测 LLM、飞书：缺失或校验失败则交互填写，直到通过（飞书可回车跳过）
			if next := setup.RunStartup(s); next != nil {
				cfg = next
			}
		}
		if cfg.LLMApiKey == "" {
			log.Fatal("请先配置 LLM：运行程序时按命令行提示输入，或设置环境变量 OPENAI_*，或访问 http://本机:PORT/setup 后重启。")
		}
	}

	mux := http.NewServeMux()

	if cfg.Mode == config.ModeWorker {
		if cfg.InternalToken == "" {
			log.Fatal("WILL_MODE=worker 时必须设置 WILL_INTERNAL_TOKEN")
		}
		mux.HandleFunc("/internal/exec", internalapi.AuthMiddleware(cfg.InternalToken, internalapi.HandleExec))
		mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true,"mode":"worker"}`))
		})
		log.Printf("WILL worker listening on %s:%s", cfg.Bind, cfg.Port)
	} else {
		mux.HandleFunc("/feishu", handleFeishu(s))
		mux.HandleFunc("/setup", handleSetup(s))
		if cfg.FeishuAppID != "" && cfg.FeishuAppSecret != "" {
			feishu.InitClient(cfg.FeishuAppID, cfg.FeishuAppSecret)
			if s != nil {
				openID, _ := s.GetConfig(store.ConfigKeyPostUpdateNotifyOpenID)
				if openID != "" {
					s.SetConfig(store.ConfigKeyPostUpdateNotifyOpenID, "")
					notes := updater.ReleaseNotes(Version)
					msg := "WILL 已更新到 v" + Version + "。"
					if notes != "" {
						msg += "\n\n本版更新说明：\n" + notes
					}
					_ = feishu.SendMessageToUser(openID, msg)
				}
			}
			if cfg.FeishuSubscribeMode == "ws" {
				go feishu.StartWSClient(cfg.FeishuAppID, cfg.FeishuAppSecret, func(openID, messageID, text string) (string, bool) {
					return processFeishuMessage(s, openID, messageID, text)
				})
			}
		}
		mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true,"mode":"` + string(cfg.Mode) + `"}`))
		})
		if cfg.InternalToken != "" {
			mux.HandleFunc("/internal/exec", internalapi.AuthMiddleware(cfg.InternalToken, internalapi.HandleExec))
		}
		log.Printf("WILL %s listening on %s:%s (db=%v)", cfg.Mode, cfg.Bind, cfg.Port, s != nil)
		if s != nil {
			go runVersionCheck(s)
			go runScheduledTasks(s)
		}
	}

	addr := cfg.Bind + ":" + cfg.Port
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func handleSetup(s *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s == nil {
			http.Error(w, "未启用本地数据库", http.StatusServiceUnavailable)
			return
		}
		// 仅允许本机或带 token 的请求
		if tok := os.Getenv("WILL_SETUP_TOKEN"); tok != "" {
			if r.Header.Get("Authorization") != "Bearer "+tok && r.URL.Query().Get("token") != tok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		} else if !isLocal(r) {
			http.Error(w, "仅允许本机访问", http.StatusForbidden)
			return
		}

		if r.Method == http.MethodGet {
			cfg := config.LoadFromStore(s)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(buildSetupHTML(cfg)))
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}

		set := func(key, v string) {
			v = strings.TrimSpace(v)
			if v != "" {
				_ = s.SetConfig(key, v)
			}
		}
		set(store.ConfigKeyLLMApiKey, r.Form.Get("llm_api_key"))
		set(store.ConfigKeyLLMBaseURL, r.Form.Get("llm_base_url"))
		set(store.ConfigKeyLLMModel, r.Form.Get("llm_model"))
		set(store.ConfigKeyFeishuAppID, r.Form.Get("feishu_app_id"))
		set(store.ConfigKeyFeishuAppSecret, r.Form.Get("feishu_app_secret"))
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("已保存。若刚配置飞书，请重启 WILL 使长连接生效；默认使用长连接，无需公网 URL。"))
	}
}

func isLocal(r *http.Request) bool {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

func buildSetupHTML(cfg *config.Config) string {
	esc := func(s string) string {
		return strings.NewReplacer("&", "&amp;", "\"", "&quot;", "<", "&lt;", ">", "&gt;").Replace(s)
	}
	apiKey := esc(cfg.LLMApiKey)
	baseURL := esc(cfg.LLMBaseURL)
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	model := esc(cfg.LLMModel)
	if model == "" {
		model = "gpt-4o-mini"
	}
	feishuID := esc(cfg.FeishuAppID)
	feishuSecret := esc(cfg.FeishuAppSecret)
	return `<!DOCTYPE html><html><head><meta charset="utf-8"><title>WILL 配置</title></head><body>
<h1>WILL 配置</h1>
<p>LLM 必填，飞书可选。保存后若修改了飞书凭证请重启 WILL 使长连接生效。</p>
<form method="post">
  <p><label>LLM API Key <input name="llm_api_key" size="60" placeholder="sk-..." value="` + apiKey + `"></label></p>
  <p><label>LLM Base URL <input name="llm_base_url" size="60" value="` + baseURL + `"></label></p>
  <p><label>LLM Model <input name="llm_model" size="30" value="` + model + `"></label></p>
  <p><label>飞书 App ID <input name="feishu_app_id" size="40" value="` + feishuID + `"></label></p>
  <p><label>飞书 App Secret <input name="feishu_app_secret" size="50" value="` + feishuSecret + `"></label></p>
  <p><button type="submit">保存</button></p>
</form>
</body></html>`
}

func handleFeishu(s *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var envelope feishu.EventEnvelope
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}

		if envelope.Type == "url_verification" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			json.NewEncoder(w).Encode(map[string]string{"challenge": envelope.Challenge})
			return
		}

		if envelope.Event == nil {
			w.WriteHeader(http.StatusOK)
			return
		}

		var ev feishu.IMMessageEvent
		if err := json.Unmarshal(envelope.Event, &ev); err != nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		if ev.Message == nil || ev.Sender == nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		if envelope.Header != nil && envelope.Header.EventType != feishu.EventIMMessageReceive {
			w.WriteHeader(http.StatusOK)
			return
		}

		openID := ""
		if ev.Sender.SenderID != nil {
			openID = ev.Sender.SenderID.OpenID
		}

		cfg := config.LoadFromStore(s)
		if cfg.FeishuAppID == "" || cfg.FeishuAppSecret == "" {
			w.WriteHeader(http.StatusOK)
			return
		}

		text := feishu.ParseTextContent(ev.Message.Content)
		if text == "" {
			w.WriteHeader(http.StatusOK)
			return
		}

		go func() {
			reply, sendReply := processFeishuMessage(s, openID, ev.Message.MessageID, text)
			if sendReply && reply != "" {
				if err := feishu.ReplyMessage(ev.Message.MessageID, reply); err != nil {
					log.Printf("feishu reply error: %v", err)
				}
			}
		}()

		w.WriteHeader(http.StatusOK)
	}
}

// processFeishuMessage 处理一条飞书消息（HTTP 与长连接共用），返回回复文案及是否发送回复
func processFeishuMessage(s *store.Store, openID, messageID, text string) (reply string, sendReply bool) {
	cfg := config.LoadFromStore(s)
	if cfg.FeishuAppID == "" || cfg.FeishuAppSecret == "" {
		return "", false
	}
	if len(cfg.FeishuAllowed) == 0 && s != nil && openID != "" {
		_ = s.AddAllowedOpenID(openID)
		cfg = config.LoadFromStore(s)
		_, _ = s.AddScheduledTask("do_version_check", "", time.Now().Add(2*time.Minute).Unix())
	}
	if len(cfg.FeishuAllowed) > 0 && !feishu.IsAllowed(openID, cfg.FeishuAllowed) {
		reply, sendReply = "未授权用户，忽略。", true
		goto done
	}
	if s != nil && tryHandleUpdateReply(s, cfg, openID, text, messageID) {
		return "", false // 已在 tryHandleUpdateReply 内回复
	}
	if s != nil {
		if rpl, ok := handlePendingConfigConfirm(s, openID, strings.TrimSpace(text)); ok {
			reply, sendReply = rpl, true
			goto done
		}
	}
	if rpl, ok := bot.HandleCommand(text, openID, s, cfg); ok {
		reply, sendReply = rpl, true
		goto done
	}
	reply, sendReply = runWithLLM(s, cfg, openID, text), true
done:
	if sendReply && s != nil && openID != "" && reply != "" {
		_ = s.AppendConversation(openID, "user", text)
		_ = s.AppendConversation(openID, "assistant", reply)
	}
	return reply, sendReply
}

const updatePromptMaxAge = 24 * time.Hour

// handlePendingConfigConfirm 若有待确认的配置变更，根据用户回复「确认」/「取消」生效或丢弃，返回 (回复, 是否已处理)
func handlePendingConfigConfirm(s *store.Store, openID, text string) (reply string, handled bool) {
	scope := "user:" + openID
	pending, ok := s.GetMemory(scope, llm.PendingConfigKey)
	if !ok || pending == "" {
		return "", false
	}
	lower := strings.ToLower(text)
	isConfirm := lower == "确认" || lower == "是" || lower == "好" || lower == "可以" || lower == "生效" || lower == "ok" || lower == "yes"
	isCancel := lower == "取消" || lower == "不" || lower == "否" || lower == "忽略" || lower == "不要" || lower == "no"
	if isConfirm {
		var m map[string]string
		if err := json.Unmarshal([]byte(pending), &m); err != nil {
			s.SetMemory(scope, llm.PendingConfigKey, "")
			return "配置解析失败，已清除待确认。", true
		}
		for k, v := range m {
			_ = s.SetConfig(k, v)
		}
		s.SetMemory(scope, llm.PendingConfigKey, "")
		return "配置已生效。", true
	}
	if isCancel {
		s.SetMemory(scope, llm.PendingConfigKey, "")
		return "已取消。", true
	}
	return "请先回复「确认」或「取消」以处理待生效的配置变更。", true
}

func tryHandleUpdateReply(s *store.Store, cfg *config.Config, openID, text, messageID string) bool {
	promptAt, _ := s.GetConfig(store.ConfigKeyUpdatePromptAt)
	if promptAt == "" {
		return false
	}
	ts, err := strconv.ParseInt(promptAt, 10, 64)
	if err != nil || time.Since(time.Unix(ts, 0)) > updatePromptMaxAge {
		return false
	}
	notifyID, _ := s.GetConfig(store.ConfigKeyUpdateNotifyOpenID)
	if notifyID != "" && notifyID != openID {
		return false
	}
	intent, err := llm.ParseUpdateReply(cfg, text)
	if err != nil {
		log.Printf("parse update reply: %v", err)
		return false
	}
	s.SetConfig(store.ConfigKeyUpdatePromptAt, "")
	s.SetConfig(store.ConfigKeyUpdateNotifyOpenID, "")
	latestVer, _ := s.GetConfig(store.ConfigKeyLatestVersion)
	assetURL := ""

	switch intent.Action {
	case "now":
		_, assetURL, err = updater.CheckLatest()
		if err != nil {
			_ = feishu.ReplyMessage(messageID, "获取更新失败 — "+err.Error())
			return true
		}
		s.SetConfig(store.ConfigKeyPostUpdateNotifyOpenID, notifyID)
		if err := updater.DownloadAndApply(assetURL); err != nil {
			s.SetConfig(store.ConfigKeyPostUpdateNotifyOpenID, "")
			_ = feishu.ReplyMessage(messageID, "更新失败 — "+err.Error())
			return true
		}
		_ = feishu.ReplyMessage(messageID, "正在更新并重启…")
		return true
	case "later":
		hours := intent.RemindHours
		if hours <= 0 {
			hours = 24
		}
		payload := `{"version":"` + latestVer + `","open_id":"` + openID + `"}`
		runAt := time.Now().Add(time.Duration(hours) * time.Hour).Unix()
		_, _ = s.AddScheduledTask("remind_update", payload, runAt)
		_ = feishu.ReplyMessage(messageID, "已记录，"+strconv.Itoa(hours)+" 小时后再提醒你。")
		return true
	default:
		_ = feishu.ReplyMessage(messageID, "本次不更新，之后有新版本会再提醒。")
		return true
	}
}

func runVersionCheck(s *store.Store) {
	tick := time.NewTicker(24 * time.Hour)
	defer tick.Stop()
	for range tick.C {
		doVersionCheck(s)
	}
}

func doVersionCheck(s *store.Store) {
	cfg := config.LoadFromStore(s)
	if cfg.FeishuAppID == "" || cfg.FeishuAppSecret == "" {
		return
	}
	allowed := s.GetAllowedOpenIDs()
	if len(allowed) == 0 {
		return
	}
	latestVer, _, err := updater.CheckLatest()
	if err != nil {
		return
	}
	current := strings.TrimPrefix(Version, "v")
	if !updater.CompareVersion(latestVer, current) {
		return
	}
	promptAt, _ := s.GetConfig(store.ConfigKeyUpdatePromptAt)
	if promptAt != "" {
		ts, _ := strconv.ParseInt(promptAt, 10, 64)
		if time.Since(time.Unix(ts, 0)) < updatePromptMaxAge {
			return
		}
	}
	s.SetConfig(store.ConfigKeyLatestVersion, latestVer)
	s.SetConfig(store.ConfigKeyUpdatePromptAt, strconv.FormatInt(time.Now().Unix(), 10))
	s.SetConfig(store.ConfigKeyUpdateNotifyOpenID, allowed[0])
	msg := "WILL 发现新版本 v" + latestVer + "，是否更新？回复「立即更新」或「稍后再说」或「X 小时后提醒」。"
	if err := feishu.SendMessageToUser(allowed[0], msg); err != nil {
		log.Printf("send update prompt: %v", err)
	}
}

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
		for _, t := range tasks {
			_ = s.DeleteScheduledTask(t.ID)
			switch t.Kind {
			case "do_version_check":
				doVersionCheck(s)
			case "remind_update":
				var payload struct {
					Version string `json:"version"`
					OpenID  string `json:"open_id"`
				}
				_ = json.Unmarshal([]byte(t.Payload), &payload)
				if payload.OpenID != "" && cfg.FeishuAppID != "" && cfg.FeishuAppSecret != "" {
					msg := "WILL 提醒：新版本 v" + payload.Version + " 仍未更新，是否现在更新？回复「立即更新」或「稍后」或「不更新」。"
					_ = feishu.SendMessageToUser(payload.OpenID, msg)
					s.SetConfig(store.ConfigKeyUpdatePromptAt, strconv.FormatInt(time.Now().Unix(), 10))
					s.SetConfig(store.ConfigKeyUpdateNotifyOpenID, payload.OpenID)
					s.SetConfig(store.ConfigKeyLatestVersion, payload.Version)
				}
			}
		}
	}
}

// runWithLLM 用 LLM 解析用户意图，再按 intent 分发或执行 command/回复
func runWithLLM(s *store.Store, cfg *config.Config, openID string, userMessage string) string {
	resp, err := llm.Call(cfg, "user:"+openID, userMessage, s)
	if err != nil {
		return "LLM 调用失败 — " + err.Error()
	}
	intent := strings.TrimSpace(strings.ToLower(resp.Intent))
	if s != nil && openID != "" {
		switch intent {
		case "todo_list":
			list, err := s.ListTodos(openID)
			if err != nil {
				return "读取待办失败: " + err.Error()
			}
			return formatTodoList(list)
		case "todo_add":
			title := strings.TrimSpace(resp.TodoTitle)
			if title == "" {
				return "未识别到待办内容，请说明要添加什么，如：帮我加个待办买牛奶"
			}
			id, err := s.AddTodo(openID, title)
			if err != nil {
				return "添加失败: " + err.Error()
			}
			return fmt.Sprintf("已添加待办 [%d] %s", id, title)
		case "todo_done":
			idStr := strings.TrimSpace(resp.TodoID)
			if idStr == "" {
				return "请说明要完成哪条待办（编号）。"
			}
			id, err := strconv.ParseInt(idStr, 10, 64)
			if err != nil {
				return "待办编号需为数字。"
			}
			ok, err := s.SetTodoStatus(id, openID, "done")
			if err != nil {
				return "操作失败: " + err.Error()
			}
			if !ok {
				return "未找到该待办或无权操作。"
			}
			return fmt.Sprintf("已将 [%d] 标为已完成。", id)
		case "todo_delete":
			idStr := strings.TrimSpace(resp.TodoID)
			if idStr == "" {
				return "请说明要删除哪条待办（编号）。"
			}
			id, err := strconv.ParseInt(idStr, 10, 64)
			if err != nil {
				return "待办编号需为数字。"
			}
			ok, err := s.DeleteTodo(id, openID)
			if err != nil {
				return "删除失败: " + err.Error()
			}
			if !ok {
				return "未找到该待办或无权操作。"
			}
			return fmt.Sprintf("已删除待办 [%d]。", id)
		case "version_check":
			reply := updater.VersionCheckReply(Version)
			if strings.Contains(reply, "发现新版本") {
				latestVer, _, _ := updater.CheckLatest()
				if latestVer != "" {
					s.SetConfig(store.ConfigKeyLatestVersion, latestVer)
					s.SetConfig(store.ConfigKeyUpdatePromptAt, strconv.FormatInt(time.Now().Unix(), 10))
					s.SetConfig(store.ConfigKeyUpdateNotifyOpenID, openID)
				}
			}
			return reply
		}
	}
	command, replyText := llm.Apply(s, openID, resp)
	if command != "" {
		out := runTask(cfg, command)
		if replyText != "" {
			return replyText + "\n\n" + out
		}
		return out
	}
	if replyText != "" {
		return replyText
	}
	return "已处理。"
}

func formatTodoList(list []store.Todo) string {
	if len(list) == 0 {
		return "当前无待办。说「添加待办 xxx」或发 /todo add xxx 添加。"
	}
	var b strings.Builder
	b.WriteString("待办列表：\n")
	for _, t := range list {
		status := "未完成"
		if t.Status == "done" {
			status = "已完成"
		}
		b.WriteString(fmt.Sprintf("[%d] %s (%s)\n", t.ID, t.Title, status))
	}
	b.WriteString("\n说「完成第1条」或「删除待办2」；或发 /todo done <id> / /todo delete <id>")
	return b.String()
}

func runTask(cfg *config.Config, instruction string) string {
	instruction = strings.TrimSpace(instruction)
	if instruction == "" {
		return "收到空指令。"
	}
	// 拦截 git 命令，避免在非 git 目录报错；检查版本请说「检查新版本」
	firstWord := instruction
	if i := strings.IndexAny(instruction, " \t"); i > 0 {
		firstWord = instruction[:i]
	}
	if strings.ToLower(firstWord) == "git" {
		return "检查版本请说「检查新版本」，按 GitHub 发布版本检查，无需 git。"
	}

	if cfg.Mode == config.ModeMain && len(cfg.WorkerURLs) > 0 && cfg.InternalToken != "" {
		workerURL := strings.TrimRight(cfg.WorkerURLs[0], "/")
		client := internalapi.NewClient(workerURL, cfg.InternalToken)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		resp, err := client.Exec(ctx, instruction, "", 300)
		if err != nil {
			return "worker 调用失败 — " + err.Error()
		}
		if resp.Error != "" {
			return resp.Stdout + "\nstderr: " + resp.Stderr + "\nerror: " + resp.Error
		}
		return resp.Stdout
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res := exec.Run(ctx, instruction, "", 5*time.Minute)
	return res.String()
}
