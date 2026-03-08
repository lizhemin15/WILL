package setup

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/yourusername/will/internal/config"
	"github.com/yourusername/will/internal/feishu"
	"github.com/yourusername/will/internal/internalapi"
	"github.com/yourusername/will/internal/llm"
	"github.com/yourusername/will/internal/store"
)

func randomShortID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}

const urlHint = "只填到域名或 /v1，不要加 /chat/completions 等路径。例如: https://api.openai.com/v1"

// RunStartup 启动时完成初始化：
//   - 首次运行（DB 中无 mode 记录且未设 WILL_MODE 环境变量）：先问通信方式
//   - 从节点模式：直接启动，LLM 配置由主节点下发
//   - 飞书模式：确保 LLM + 飞书凭证有效
func RunStartup(s *store.Store) *config.Config {
	if s == nil {
		return nil
	}

	cfg := config.LoadFromStore(s)

	// 从节点模式（来自 DB 或 WILL_MODE 环境变量）：无需额外配置
	if cfg.Mode == config.ModeWorker {
		fmt.Fprintln(os.Stdout, "[WILL] 从节点模式启动，配置已就绪。")
		return cfg
	}

	// 判断是否首次运行：DB 里没有 mode 记录，且未通过环境变量指定模式
	_, modeInDB := s.GetConfig(store.ConfigKeyMode)
	if !modeInDB && os.Getenv("WILL_MODE") == "" {
		reader := bufio.NewReader(os.Stdin)
		fmt.Fprintln(os.Stdout, "欢迎使用 WILL！首次运行，请完成初始配置。")
		fmt.Fprintln(os.Stdout, "")
		fmt.Fprintln(os.Stdout, "选择通信方式：")
		fmt.Fprintln(os.Stdout, "  [1] 飞书通信（主节点 / 独立节点，直接通过飞书消息控制）")
		fmt.Fprintln(os.Stdout, "  [2] 机器人间通信（从节点，绑定到已有的飞书主节点机器人）")
		fmt.Fprint(os.Stdout, "请选择 [1/2]（默认 1）: ")
		line, _ := reader.ReadString('\n')
		if strings.TrimSpace(line) == "2" {
			return ensurePeerPairing(s, reader)
		}
		// 选飞书通信即为主节点（main），同时启动 HTTP 服务供从节点配对和 WebSocket 连接
		_ = s.SetConfig(store.ConfigKeyMode, string(config.ModeMain))
	}

	// 飞书模式：确保 LLM 和飞书凭证有效
	_ = ensureLLM(s)
	ensureFeishuOrSkip(s)
	return config.LoadFromStore(s)
}

// ensurePeerPairing 引导用户完成与主节点的配对，写入 worker 模式所需配置
func ensurePeerPairing(s *store.Store, reader *bufio.Reader) *config.Config {
	fmt.Fprintln(os.Stdout, "")
	fmt.Fprintln(os.Stdout, "机器人间配对步骤：")
	fmt.Fprintln(os.Stdout, "  1. 在主节点机器人的飞书对话中发送 /pair")
	fmt.Fprintln(os.Stdout, "  2. 将显示的配对码填入下方")
	fmt.Fprintln(os.Stdout, "")

	fmt.Fprint(os.Stdout, "为本节点取一个名字（如「家里的电脑」「服务器A」）: ")
	nameLine, _ := reader.ReadString('\n')
	workerName := strings.TrimSpace(nameLine)
	if workerName == "" {
		workerName = "节点-" + randomShortID()
		fmt.Fprintln(os.Stdout, "未输入名称，自动命名为:", workerName)
	}

	var mainURL string
	for {
		fmt.Fprint(os.Stdout, "主节点地址（如 http://192.168.1.5:3000）: ")
		line, _ := reader.ReadString('\n')
		mainURL = strings.TrimSpace(line)
		if mainURL != "" {
			break
		}
		fmt.Fprintln(os.Stdout, "地址不能为空。")
	}

	var pairToken string
	for {
		fmt.Fprint(os.Stdout, "配对码（如 WILL-XXXXXXXX）: ")
		line, _ := reader.ReadString('\n')
		pairToken = strings.TrimSpace(line)
		if pairToken != "" {
			break
		}
		fmt.Fprintln(os.Stdout, "配对码不能为空。")
	}

	fmt.Fprintln(os.Stdout, "正在连接主节点配对…")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp, err := internalapi.PairWithMain(ctx, mainURL, pairToken, workerName)
	if err != nil {
		fmt.Fprintln(os.Stdout, "配对失败:", err)
		fmt.Fprintln(os.Stdout, "请检查主节点地址和配对码后重新运行。")
		os.Exit(1)
	}
	if !resp.OK {
		fmt.Fprintln(os.Stdout, "配对失败:", resp.Error)
		os.Exit(1)
	}

	_ = s.SetConfig(store.ConfigKeyMode, string(config.ModeWorker))
	_ = s.SetConfig(store.ConfigKeyInternalToken, resp.WorkerToken)
	_ = s.SetConfig(store.ConfigKeyMainURL, mainURL)
	_ = s.SetConfig(store.ConfigKeyWorkerName, workerName)
	// LLM 配置由主节点下发（配对时获取，连接后通过 WebSocket 同步）
	if resp.LLMApiKey != "" {
		_ = s.SetConfig(store.ConfigKeyLLMApiKey, resp.LLMApiKey)
	}
	if resp.LLMBaseURL != "" {
		_ = s.SetConfig(store.ConfigKeyLLMBaseURL, resp.LLMBaseURL)
	}
	if resp.LLMModel != "" {
		_ = s.SetConfig(store.ConfigKeyLLMModel, resp.LLMModel)
	}

	fmt.Fprintf(os.Stdout, "配对成功！节点「%s」已设为从节点模式，启动后将自动连接主节点并同步配置。\n", workerName)
	return config.LoadFromStore(s)
}

func ensureLLM(s *store.Store) *config.Config {
	reader := bufio.NewReader(os.Stdin)
	for {
		cfg := config.LoadFromStore(s)
		if cfg.LLMApiKey == "" && os.Getenv("OPENAI_API_KEY") == "" {
			fmt.Fprintln(os.Stdout, "WILL 未检测到 LLM 配置，请按提示输入（必填）。")
			fmt.Fprintln(os.Stdout, "")
			promptLLMOnce(s, reader)
			if err := llm.TestConfig(cfgFromStore(s)); err != nil {
				fmt.Fprintln(os.Stdout, "LLM 校验失败:", err)
				continue
			}
			fmt.Fprintln(os.Stdout, "LLM 校验通过。")
			break
		}
		if err := llm.TestConfig(cfg); err != nil {
			fmt.Fprintln(os.Stdout, "LLM 校验失败:", err)
			fmt.Fprintln(os.Stdout, "请重新输入正确的 LLM 配置。")
			promptLLMOnce(s, reader)
			if err := llm.TestConfig(cfgFromStore(s)); err != nil {
				fmt.Fprintln(os.Stdout, "LLM 校验仍失败:", err)
				continue
			}
		}
		fmt.Fprintln(os.Stdout, "LLM 校验通过。")
		break
	}
	return config.LoadFromStore(s)
}

func cfgFromStore(s *store.Store) *config.Config {
	key, _ := s.GetConfig(store.ConfigKeyLLMApiKey)
	baseURL, _ := s.GetConfig(store.ConfigKeyLLMBaseURL)
	model, _ := s.GetConfig(store.ConfigKeyLLMModel)
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	if model == "" {
		model = "gpt-4o-mini"
	}
	return &config.Config{LLMApiKey: key, LLMBaseURL: baseURL, LLMModel: model}
}

func promptLLMOnce(s *store.Store, reader *bufio.Reader) {
	fmt.Fprint(os.Stdout, "LLM API Key (必填): ")
	key, _ := reader.ReadString('\n')
	key = strings.TrimSpace(key)
	if key == "" {
		fmt.Fprintln(os.Stdout, "未输入 API Key，退出。")
		os.Exit(1)
	}
	fmt.Fprintf(os.Stdout, "LLM Base URL (%s): ", urlHint)
	baseURL, _ := reader.ReadString('\n')
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	fmt.Fprint(os.Stdout, "LLM Model (回车默认 gpt-4o-mini): ")
	model, _ := reader.ReadString('\n')
	model = strings.TrimSpace(model)
	if model == "" {
		model = "gpt-4o-mini"
	}
	_ = s.SetConfig(store.ConfigKeyLLMApiKey, key)
	_ = s.SetConfig(store.ConfigKeyLLMBaseURL, baseURL)
	_ = s.SetConfig(store.ConfigKeyLLMModel, model)
	fmt.Fprintln(os.Stdout, "已保存，正在校验…")
}

func ensureFeishuOrSkip(s *store.Store) {
	reader := bufio.NewReader(os.Stdin)
	for {
		cfg := config.LoadFromStore(s)
		if cfg.FeishuAppID == "" && cfg.FeishuAppSecret == "" {
			fmt.Fprintln(os.Stdout, "")
			fmt.Fprint(os.Stdout, "飞书 App ID (直接回车跳过): ")
			appID, _ := reader.ReadString('\n')
			appID = strings.TrimSpace(appID)
			if appID == "" {
				fmt.Fprintln(os.Stdout, "已跳过飞书配置。")
				return
			}
			fmt.Fprint(os.Stdout, "飞书 App Secret: ")
			appSecret, _ := reader.ReadString('\n')
			appSecret = strings.TrimSpace(appSecret)
			_ = s.SetConfig(store.ConfigKeyFeishuAppID, appID)
			_ = s.SetConfig(store.ConfigKeyFeishuAppSecret, appSecret)
			fmt.Fprintln(os.Stdout, "正在校验飞书凭证…")
			if err := feishu.TestCredentials(appID, appSecret); err != nil {
				fmt.Fprintln(os.Stdout, "飞书校验失败:", err)
				continue
			}
			fmt.Fprintln(os.Stdout, "飞书校验通过。")
			return
		}
		if err := feishu.TestCredentials(cfg.FeishuAppID, cfg.FeishuAppSecret); err != nil {
			fmt.Fprintln(os.Stdout, "飞书校验失败:", err)
			fmt.Fprint(os.Stdout, "飞书 App ID (直接回车跳过): ")
			appID, _ := reader.ReadString('\n')
			appID = strings.TrimSpace(appID)
			if appID == "" {
				_ = s.SetConfig(store.ConfigKeyFeishuAppID, "")
				_ = s.SetConfig(store.ConfigKeyFeishuAppSecret, "")
				fmt.Fprintln(os.Stdout, "已清除飞书配置并跳过。")
				return
			}
			fmt.Fprint(os.Stdout, "飞书 App Secret: ")
			appSecret, _ := reader.ReadString('\n')
			appSecret = strings.TrimSpace(appSecret)
			_ = s.SetConfig(store.ConfigKeyFeishuAppID, appID)
			_ = s.SetConfig(store.ConfigKeyFeishuAppSecret, appSecret)
			if err := feishu.TestCredentials(appID, appSecret); err != nil {
				fmt.Fprintln(os.Stdout, "飞书校验仍失败:", err)
				continue
			}
			fmt.Fprintln(os.Stdout, "飞书校验通过。")
		} else {
			fmt.Fprintln(os.Stdout, "飞书校验通过。")
		}
		return
	}
}
