// Package peer 实现主节点（Hub）与从节点（WorkerClient）之间的 WebSocket 通信。
// 从节点主动连接主节点，无需暴露端口，天然穿透 NAT/防火墙。
package peer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/yourusername/will/internal/store"
)

// cmdMsg 主节点发给从节点的指令
type cmdMsg struct {
	ID         string `json:"id"`
	Command    string `json:"cmd"`
	WorkDir    string `json:"work_dir"`
	TimeoutSec int    `json:"timeout_sec"`
}

// ResultMsg 从节点返回给主节点的执行结果（供 main.go 复用）
type ResultMsg struct {
	ID       string `json:"id"`
	OK       bool   `json:"ok"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
}

// workerConn 表示一条已连接的从节点 WebSocket 连接
type workerConn struct {
	send chan []byte
	hub  *Hub
}

// Hub 维护全部从节点的 WebSocket 连接，并提供 Exec 方法向从节点派发命令
type Hub struct {
	s *store.Store // 动态读取 internal_token（配对后热更新）

	mu      sync.RWMutex
	workers []*workerConn

	pendingMu sync.Mutex
	pending   map[string]chan *ResultMsg

	upgrader websocket.Upgrader
}

// NewHub 创建 Hub；store 用于运行时读取最新 token
func NewHub(s *store.Store) *Hub {
	return &Hub{
		s:       s,
		pending: make(map[string]chan *ResultMsg),
		upgrader: websocket.Upgrader{
			HandshakeTimeout: 10 * time.Second,
			CheckOrigin:      func(r *http.Request) bool { return true },
		},
	}
}

// token 从 store 动态读取，支持配对后不重启生效
func (h *Hub) token() string {
	if h.s != nil {
		if tok, ok := h.s.GetConfig(store.ConfigKeyInternalToken); ok && tok != "" {
			return tok
		}
	}
	return ""
}

// WorkerCount 返回当前在线从节点数
func (h *Hub) WorkerCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.workers)
}

// ServeWS 是注册到 HTTP mux 的 WebSocket 升级处理函数
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	tok := h.token()
	if tok == "" {
		http.Error(w, `{"ok":false,"error":"token not configured"}`, http.StatusServiceUnavailable)
		return
	}
	auth := r.Header.Get("Authorization")
	if auth != "Bearer "+tok {
		http.Error(w, `{"ok":false,"error":"invalid token"}`, http.StatusUnauthorized)
		return
	}

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[hub] WebSocket 升级失败: %v", err)
		return
	}

	wc := &workerConn{send: make(chan []byte, 64), hub: h}

	h.mu.Lock()
	h.workers = append(h.workers, wc)
	h.mu.Unlock()
	log.Printf("[hub] 从节点已连接，当前共 %d 个", h.WorkerCount())

	go wc.writePump(conn)
	wc.readPump(conn) // 阻塞直到连接关闭

	h.mu.Lock()
	for i, c := range h.workers {
		if c == wc {
			h.workers = append(h.workers[:i], h.workers[i+1:]...)
			break
		}
	}
	h.mu.Unlock()
	log.Printf("[hub] 从节点断开，剩余 %d 个", h.WorkerCount())
}

func (wc *workerConn) writePump(conn *websocket.Conn) {
	defer conn.Close()
	for msg := range wc.send {
		if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			return
		}
	}
}

func (wc *workerConn) readPump(conn *websocket.Conn) {
	defer conn.Close()
	conn.SetReadLimit(8 * 1024 * 1024)
	conn.SetReadDeadline(time.Time{}) // 无超时，长连接
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var res ResultMsg
		if err := json.Unmarshal(msg, &res); err != nil {
			continue
		}
		wc.hub.pendingMu.Lock()
		ch, ok := wc.hub.pending[res.ID]
		wc.hub.pendingMu.Unlock()
		if ok {
			select {
			case ch <- &res:
			default:
			}
		}
	}
}

// Exec 选取一个在线从节点执行命令并返回结果；ctx 超时则提前返回
func (h *Hub) Exec(ctx context.Context, command, workDir string, timeoutSec int) (*ResultMsg, error) {
	h.mu.RLock()
	var wc *workerConn
	if len(h.workers) > 0 {
		wc = h.workers[0] // 简单选第一个；后续可扩展轮询
	}
	h.mu.RUnlock()

	if wc == nil {
		return nil, fmt.Errorf("暂无在线从节点，请先启动并配对从节点")
	}

	if timeoutSec <= 0 {
		timeoutSec = 300
	}
	id := randomID()
	ch := make(chan *ResultMsg, 1)

	h.pendingMu.Lock()
	h.pending[id] = ch
	h.pendingMu.Unlock()
	defer func() {
		h.pendingMu.Lock()
		delete(h.pending, id)
		h.pendingMu.Unlock()
	}()

	msg, _ := json.Marshal(cmdMsg{
		ID:         id,
		Command:    command,
		WorkDir:    workDir,
		TimeoutSec: timeoutSec,
	})
	select {
	case wc.send <- msg:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// StatusLine 返回从节点连接状态描述
func (h *Hub) StatusLine() string {
	n := h.WorkerCount()
	if n == 0 {
		return "从节点：无在线节点（使用 /pair 获取配对码）"
	}
	return fmt.Sprintf("从节点：%d 个在线", n)
}

// HubHandler 返回注册到 /internal/ws 的 HTTP 处理函数
func (h *Hub) HubHandler() http.HandlerFunc {
	return h.ServeWS
}

// FormatResult 将 ResultMsg 格式化为 runTask 返回字符串
func FormatResult(r *ResultMsg) string {
	if r.Error != "" {
		out := strings.TrimSpace(r.Stdout)
		if r.Stderr != "" {
			out += "\nstderr: " + strings.TrimSpace(r.Stderr)
		}
		out += "\nerror: " + r.Error
		return strings.TrimSpace(out)
	}
	out := r.Stdout
	if r.Stderr != "" && r.ExitCode != 0 {
		out += "\nstderr: " + r.Stderr
	}
	return strings.TrimSpace(out)
}
