package peer

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/yourusername/will/internal/exec"
)

// WorkerClient 在从节点上运行，主动连接主节点 WebSocket 并执行收到的命令。
// 连接断开后自动重试，无需从节点暴露任何端口。
type WorkerClient struct {
	MainURL string // 主节点地址，如 http://192.168.1.5:3000
	Token   string // 与主节点共享的 internal_token
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

		// 连接超过 5 秒后断开则重置退避（说明曾正常工作）
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

	// writeMu 保证 gorilla WebSocket 的写操作序列化
	var writeMu sync.Mutex
	writeMsg := func(data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(websocket.TextMessage, data)
	}

	conn.SetReadLimit(4 * 1024 * 1024)
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var cmd cmdMsg
		if err := json.Unmarshal(msg, &cmd); err != nil {
			log.Printf("[worker] 收到无法解析的消息: %v", err)
			continue
		}
		go c.handleCmd(ctx, writeMsg, cmd)
	}
}

func (c *WorkerClient) handleCmd(ctx context.Context, writeMsg func([]byte) error, cmd cmdMsg) {
	timeout := time.Duration(cmd.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res := exec.Run(execCtx, cmd.Command, cmd.WorkDir, timeout)

	result := ResultMsg{
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
		log.Printf("[worker] 发送结果失败: %v", err)
	}
}
