package peer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/yourusername/will/internal/exec"
	"github.com/yourusername/will/internal/store"
	"github.com/yourusername/will/internal/updater"
)

// WorkerClient 在从节点上运行，主动连接主节点 WebSocket 并执行收到的命令。
// 连接断开后自动重试，无需从节点暴露任何端口。
type WorkerClient struct {
	MainURL string       // 主节点地址，如 http://192.168.1.5:3000
	Token   string       // 与主节点共享的 internal_token
	Name    string       // 本节点名称，连接后通过 hello 消息注册
	Store   *store.Store // 用于保存主节点下发的 LLM 配置
}

// Start 启动 WebSocket 客户端，持续连接主节点并处理命令；ctx 取消则退出
func (c *WorkerClient) Start(ctx context.Context) {
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		start := time.Now()
		if err := c.connect(ctx); err != nil {
			log.Printf("[worker] 连接主节点失败: %v", err)
		} else {
			log.Printf("[worker] 与主节点的连接已断开")
		}

		if time.Since(start) > 5*time.Second {
			backoff = time.Second
		}

		log.Printf("[worker] %s 后重试…", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (c *WorkerClient) wsURL() string {
	u := strings.TrimRight(c.MainURL, "/")
	u = strings.Replace(u, "https://", "wss://", 1)
	u = strings.Replace(u, "http://", "ws://", 1)
	return u + "/internal/ws"
}

func (c *WorkerClient) connect(ctx context.Context) error {
	header := http.Header{"Authorization": {"Bearer " + c.Token}}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, c.wsURL(), header)
	if err != nil {
		return err
	}
	defer conn.Close()
	log.Printf("[worker] 已连接主节点 %s，等待指令…", c.MainURL)

	// writeMu 保证 gorilla WebSocket 写操作序列化
	var writeMu sync.Mutex
	writeMsg := func(data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(websocket.TextMessage, data)
	}

	// 向主节点注册名称及平台信息（主节点据此下载对应架构的更新包）
	if c.Name != "" {
		helloData, _ := json.Marshal(HelloMsg{
			Type: TypeHello,
			Name: c.Name,
			OS:   runtime.GOOS,
			Arch: runtime.GOARCH,
		})
		if err := writeMsg(helloData); err != nil {
			return err
		}
	}

	// 64 MB：足以容纳主节点推送的 zip 更新包（base64 后约 1.33 倍原始大小）
	conn.SetReadLimit(64 * 1024 * 1024)
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var base struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(msg, &base); err != nil {
			continue
		}
		switch base.Type {
		case TypeConfig:
			// 主节点下发 LLM 配置，保存到本地 store
			var cfg ConfigMsg
			if err := json.Unmarshal(msg, &cfg); err == nil && c.Store != nil {
				if cfg.LLMApiKey != "" {
					_ = c.Store.SetConfig(store.ConfigKeyLLMApiKey, cfg.LLMApiKey)
				}
				if cfg.LLMBaseURL != "" {
					_ = c.Store.SetConfig(store.ConfigKeyLLMBaseURL, cfg.LLMBaseURL)
				}
				if cfg.LLMModel != "" {
					_ = c.Store.SetConfig(store.ConfigKeyLLMModel, cfg.LLMModel)
				}
				log.Printf("[worker] 已从主节点获取 LLM 配置（模型: %s）", cfg.LLMModel)
			}
		case TypeExec, "": // "" 兼容旧版
			var cmd ExecMsg
			if err := json.Unmarshal(msg, &cmd); err != nil {
				continue
			}
			go c.handleExec(ctx, writeMsg, cmd)
		case TypeUpdatePayload:
			var upd UpdatePayloadMsg
			if err := json.Unmarshal(msg, &upd); err != nil {
				log.Printf("[worker] 解析更新包消息失败: %v", err)
				continue
			}
			go c.handleUpdatePayload(writeMsg, upd)
		}
	}
}

func (c *WorkerClient) handleExec(ctx context.Context, writeMsg func([]byte) error, cmd ExecMsg) {
	timeout := time.Duration(cmd.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res := exec.Run(execCtx, cmd.Command, cmd.WorkDir, timeout)

	result := ResultMsg{
		Type:     TypeResult,
		ID:       cmd.ID,
		OK:       res.Err == nil && res.ExitCode == 0,
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		ExitCode: res.ExitCode,
	}
	if res.Err != nil {
		result.Error = res.Err.Error()
	}
	data, _ := json.Marshal(result)
	if err := writeMsg(data); err != nil {
		log.Printf("[worker] 发送执行结果失败: %v", err)
	}
}

// handleUpdatePayload 接收主节点推送的 zip 包并直接应用，无需访问 GitHub
func (c *WorkerClient) handleUpdatePayload(writeMsg func([]byte) error, upd UpdatePayloadMsg) {
	log.Printf("[worker] 收到主节点推送的更新包 v%s，正在应用…", upd.Version)

	zipData, err := base64.StdEncoding.DecodeString(upd.ZipB64)
	if err != nil {
		errMsg := "解码更新包失败: " + err.Error()
		log.Printf("[worker] %s", errMsg)
		data, _ := json.Marshal(UpdateResultMsg{Type: TypeUpdateResult, OK: false, Error: errMsg})
		_ = writeMsg(data)
		return
	}

	// 先回报成功，再应用（进程即将退出，消息发出后立刻替换）
	data, _ := json.Marshal(UpdateResultMsg{Type: TypeUpdateResult, OK: true})
	_ = writeMsg(data)
	time.Sleep(200 * time.Millisecond)

	if err := updater.ApplyFromBytes(zipData); err != nil {
		log.Printf("[worker] 应用更新失败: %v", err)
	}
}
