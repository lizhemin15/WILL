package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
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
)

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
			// SQLite 中无 LLM 配置且无环境变量时，在命令行与用户交互并写入
			if next := setup.PromptLLMIfMissing(s); next != nil {
				cfg = next
			}
		}
		if cfg.LLMApiKey == "" {
			log.Fatal("请先配置 LLM：运行程序时在命令行按提示输入，或设置环境变量 OPENAI_API_KEY、OPENAI_BASE_URL、OPENAI_MODEL，或访问 http://本机:PORT/setup 写入后重启。")
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
		mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true,"mode":"` + string(cfg.Mode) + `"}`))
		})
		if cfg.InternalToken != "" {
			mux.HandleFunc("/internal/exec", internalapi.AuthMiddleware(cfg.InternalToken, internalapi.HandleExec))
		}
		log.Printf("WILL %s listening on %s:%s (db=%v)", cfg.Mode, cfg.Bind, cfg.Port, s != nil)
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
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(setupHTML))
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
		w.Write([]byte("已保存。请将飞书事件订阅请求 URL 指向本服务的 /feishu，然后可在飞书中与 WILL 对话。"))
	}
}

func isLocal(r *http.Request) bool {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

const setupHTML = `<!DOCTYPE html><html><head><meta charset="utf-8"><title>WILL 配置</title></head><body>
<h1>WILL 首次配置</h1>
<p>请先填写 LLM（必填），飞书可选。保存后可在飞书对话中通过自然语言继续修改配置。</p>
<form method="post">
  <p><label>LLM API Key <input name="llm_api_key" size="60" placeholder="sk-..."></label></p>
  <p><label>LLM Base URL <input name="llm_base_url" size="60" value="https://api.openai.com/v1"></label></p>
  <p><label>LLM Model <input name="llm_model" size="30" value="gpt-4o-mini"></label></p>
  <p><label>飞书 App ID <input name="feishu_app_id" size="40"></label></p>
  <p><label>飞书 App Secret <input name="feishu_app_secret" size="50"></label></p>
  <p><button type="submit">保存</button></p>
</form>
</body></html>`

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

		if len(cfg.FeishuAllowed) == 0 && s != nil && openID != "" {
			_ = s.AddAllowedOpenID(openID)
			cfg = config.LoadFromStore(s)
		}

		if len(cfg.FeishuAllowed) > 0 && !feishu.IsAllowed(openID, cfg.FeishuAllowed) {
			_ = feishu.ReplyMessage(cfg.FeishuAppID, cfg.FeishuAppSecret, ev.Message.MessageID, "WILL：未授权用户，忽略。")
			w.WriteHeader(http.StatusOK)
			return
		}

		text := feishu.ParseTextContent(ev.Message.Content)
		if text == "" {
			w.WriteHeader(http.StatusOK)
			return
		}

		go func() {
			var reply string
			if rpl, ok := bot.HandleCommand(text, openID, s, cfg); ok {
				reply = "WILL：\n" + rpl
			} else {
				reply = runWithLLM(s, cfg, openID, text)
			}
			if err := feishu.ReplyMessage(cfg.FeishuAppID, cfg.FeishuAppSecret, ev.Message.MessageID, reply); err != nil {
				log.Printf("feishu reply error: %v", err)
			}
		}()

		w.WriteHeader(http.StatusOK)
	}
}

// runWithLLM 用 LLM 解析用户意图，写入 config/memory，执行 command，再回复
func runWithLLM(s *store.Store, cfg *config.Config, openID string, userMessage string) string {
	resp, err := llm.Call(cfg, "user:"+openID, userMessage)
	if err != nil {
		return "WILL：LLM 调用失败 — " + err.Error()
	}
	command, replyText := llm.Apply(s, openID, resp)
	if command != "" {
		out := runTask(cfg, command)
		if replyText != "" {
			return "WILL：\n" + replyText + "\n\n" + out
		}
		return out
	}
	if replyText != "" {
		return "WILL：\n" + replyText
	}
	return "WILL：已处理。"
}

func runTask(cfg *config.Config, instruction string) string {
	instruction = strings.TrimSpace(instruction)
	if instruction == "" {
		return "WILL：收到空指令。"
	}

	if cfg.Mode == config.ModeMain && len(cfg.WorkerURLs) > 0 && cfg.InternalToken != "" {
		workerURL := strings.TrimRight(cfg.WorkerURLs[0], "/")
		client := internalapi.NewClient(workerURL, cfg.InternalToken)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		resp, err := client.Exec(ctx, instruction, "", 300)
		if err != nil {
			return "WILL：worker 调用失败 — " + err.Error()
		}
		if resp.Error != "" {
			return "WILL：\n" + resp.Stdout + "\nstderr: " + resp.Stderr + "\nerror: " + resp.Error
		}
		return "WILL：\n" + resp.Stdout
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res := exec.Run(ctx, instruction, "", 5*time.Minute)
	return "WILL：\n" + res.String()
}
