package bot

import (
	"strings"

	"github.com/yourusername/will/internal/config"
	"github.com/yourusername/will/internal/store"
)

// HandleCommand 处理 / 开头的配置与记忆命令，返回回复文案；若不是命令或未处理则返回空字符串。
func HandleCommand(text string, openID string, s *store.Store, cfg *config.Config) (reply string, handled bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", false
	}
	parts := strings.SplitN(text, " ", 4)
	cmd := strings.ToLower(strings.TrimPrefix(parts[0], "/"))
	if len(parts) < 2 {
		if cmd == "help" || cmd == "" {
			return cmdHelp(), true
		}
		if cmd == "config" {
			return cmdConfigGet(s, cfg), true
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
	default:
		return "", false
	}
}

func cmdHelp() string {
	return `WILL 命令:
/allow me          — 将当前用户加入授权列表
/config get        — 查看当前配置（密钥脱敏）
/config <key> <v>  — 设置 key（含 feishu_app_id、feishu_app_secret、llm_*、mode、worker_urls 等）
/memory set <k> <v> — 记录记忆
/memory get <k>     — 读取记忆
/memory list       — 列出当前 scope 的记忆
自然语言也可：直接说「把飞书 app id 设为 xxx」或「执行 ls」，由 LLM 解析后写入配置或执行命令。`
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
		"llm_model":          store.ConfigKeyLLMModel,
	}
	configKey, ok := allowedKeys[key]
	if !ok {
		return "未知 key，可用: mode, internal_token, worker_urls, port, bind, feishu_app_id, feishu_app_secret, llm_api_key, llm_base_url, llm_model"
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
