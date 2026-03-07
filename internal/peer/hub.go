// Package peer 实现主节点（Hub）与从节点（WorkerClient）之间的 WebSocket 通信。
// 从节点主动连接主节点，无需暴露端口，天然穿透 NAT/防火墙。
package peer

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/yourusername/will/internal/store"
	"github.com/yourusername/will/internal/updater"
)

// workerConn 表示一条已连接的从节点 WebSocket 连接
type workerConn struct {
	name string // 节点名称，通过 hello 消息注册
	os   string // runtime.GOOS（从节点上报，用于下载对应架构包）
	arch string // runtime.GOARCH
	send chan []byte
	hub  *Hub
}

// Hub 维护全部从节点的 WebSocket 连接，并提供命令派发能力
type Hub struct {
	s *store.Store // 动态读取 token 及 LLM 配置

	mu      sync.RWMutex
	workers []*workerConn
	byName  map[string]*workerConn // 已命名节点索引

	pendingMu sync.Mutex
	pending   map[string]chan *ResultMsg

	upgrader websocket.Upgrader
}

// NewHub 创建 Hub；store 用于运行时读取最新 token 和 LLM 配置
func NewHub(s *store.Store) *Hub {
	return &Hub{
		s:       s,
		pending: make(map[string]chan *ResultMsg),
		byName:  make(map[string]*workerConn),
		upgrader: websocket.Upgrader{
			HandshakeTimeout: 10 * time.Second,
			CheckOrigin:      func(r *http.Request) bool { return true },
		},
	}
}

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

// ListWorkers 返回所有在线从节点信息
func (h *Hub) ListWorkers() []WorkerInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]WorkerInfo, 0, len(h.workers))
	for _, wc := range h.workers {
		out = append(out, WorkerInfo{Name: wc.name})
	}
	return out
}

// WorkersText 返回从节点列表的可读文本（供 Feishu 回复）
func (h *Hub) WorkersText() string {
	workers := h.ListWorkers()
	if len(workers) == 0 {
		return "当前无在线从节点。\n发送 /pair 获取配对码，在目标机器上部署 WILL 并选择「机器人间通信」模式后自动连接。"
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("已连接从节点（%d 个）：\n", len(workers)))
	for i, w := range workers {
		name := w.Name
		if name == "" {
			name = "(未命名)"
		}
		b.WriteString(fmt.Sprintf("[%d] %s\n", i+1, name))
	}
	b.WriteString("\n可说「让<名称>执行 ls -la」或「让<名称>升级版本」")
	return strings.TrimRight(b.String(), "\n")
}

// StatusLine 返回一行状态描述（用于 /status 命令）
func (h *Hub) StatusLine() string {
	workers := h.ListWorkers()
	if len(workers) == 0 {
		return "从节点：无在线节点（发送 /pair 获取配对码）"
	}
	var names []string
	for _, w := range workers {
		if w.Name != "" {
			names = append(names, w.Name)
		} else {
			names = append(names, "(未命名)")
		}
	}
	return fmt.Sprintf("从节点（%d 个在线）：%s", len(workers), strings.Join(names, "、"))
}

// ServeWS 是注册到 /internal/ws 的 WebSocket 升级处理函数
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	tok := h.token()
	if tok == "" {
		http.Error(w, `{"ok":false,"error":"token not configured"}`, http.StatusServiceUnavailable)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+tok {
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
	if wc.name != "" {
		delete(h.byName, wc.name)
		log.Printf("[hub] 从节点「%s」已断开，剩余 %d 个", wc.name, len(h.workers))
	} else {
		log.Printf("[hub] 从节点（未命名）已断开，剩余 %d 个", len(h.workers))
	}
	h.mu.Unlock()
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
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var base struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(msg, &base); err != nil {
			continue
		}
		switch base.Type {
		case TypeHello:
			var hello HelloMsg
			if err := json.Unmarshal(msg, &hello); err == nil && hello.Name != "" {
				wc.hub.registerName(wc, hello.Name, hello.OS, hello.Arch)
			}
		case TypeResult, "": // "" 兼容旧版无 type 字段的消息
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
		case TypeUpdateResult:
			var ur UpdateResultMsg
			if err := json.Unmarshal(msg, &ur); err == nil {
				if ur.OK {
					log.Printf("[hub] 从节点「%s」升级成功，即将重连", wc.name)
				} else {
					log.Printf("[hub] 从节点「%s」升级失败: %s", wc.name, ur.Error)
				}
			}
		}
	}
}

// registerName 注册节点名称和平台信息，并立即下发 LLM 配置
func (h *Hub) registerName(wc *workerConn, name, osName, arch string) {
	h.mu.Lock()
	wc.name = name
	if osName != "" {
		wc.os = osName
	}
	if arch != "" {
		wc.arch = arch
	}
	h.byName[name] = wc
	h.mu.Unlock()
	log.Printf("[hub] 从节点注册: 名称=%q os=%s arch=%s", name, wc.os, wc.arch)
	h.sendConfig(wc)
}

// sendConfig 向指定从节点下发主节点 LLM 配置
func (h *Hub) sendConfig(wc *workerConn) {
	if h.s == nil {
		return
	}
	apiKey, _ := h.s.GetConfig(store.ConfigKeyLLMApiKey)
	baseURL, _ := h.s.GetConfig(store.ConfigKeyLLMBaseURL)
	model, _ := h.s.GetConfig(store.ConfigKeyLLMModel)
	if apiKey == "" {
		return
	}
	msg, _ := json.Marshal(ConfigMsg{
		Type:       TypeConfig,
		LLMApiKey:  apiKey,
		LLMBaseURL: baseURL,
		LLMModel:   model,
	})
	select {
	case wc.send <- msg:
	default:
	}
}

// pickWorker 按名称查找节点；name 为空则取第一个可用节点
func (h *Hub) pickWorker(name string) (*workerConn, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if name != "" {
		wc, ok := h.byName[name]
		if !ok {
			// 列出可用节点名称方便提示
			var avail []string
			for n := range h.byName {
				avail = append(avail, n)
			}
			if len(avail) == 0 {
				return nil, fmt.Errorf("从节点「%s」不在线，当前无任何已命名节点", name)
			}
			return nil, fmt.Errorf("从节点「%s」不在线，可用节点：%s", name, strings.Join(avail, "、"))
		}
		return wc, nil
	}
	if len(h.workers) == 0 {
		return nil, fmt.Errorf("暂无在线从节点，请先配对并启动从节点")
	}
	return h.workers[0], nil
}

// sendExec 向指定 workerConn 派发命令并等待结果
func (h *Hub) sendExec(ctx context.Context, wc *workerConn, command, workDir string, timeoutSec int) (*ResultMsg, error) {
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

	msg, _ := json.Marshal(ExecMsg{
		Type:       TypeExec,
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

// Exec 向任意可用从节点派发命令（取第一个）
func (h *Hub) Exec(ctx context.Context, command, workDir string, timeoutSec int) (*ResultMsg, error) {
	return h.ExecNamed(ctx, "", command, workDir, timeoutSec)
}

// ExecNamed 向指定名称的从节点派发命令；name 为空时取第一个可用节点
func (h *Hub) ExecNamed(ctx context.Context, name, command, workDir string, timeoutSec int) (*ResultMsg, error) {
	wc, err := h.pickWorker(name)
	if err != nil {
		return nil, err
	}
	return h.sendExec(ctx, wc, command, workDir, timeoutSec)
}

// TriggerUpdate 由主节点从 GitHub 下载对应从节点平台的 zip 包，再经 WebSocket 推送给从节点。
// 从节点无需直接访问 GitHub，解决网络不稳定问题。
func (h *Hub) TriggerUpdate(ctx context.Context, name string) error {
	wc, err := h.pickWorker(name)
	if err != nil {
		return err
	}

	// 使用从节点上报的平台信息；若未上报则使用主节点自身平台（通常相同）
	osName := wc.os
	arch := wc.arch
	if osName == "" {
		osName = runtime.GOOS
	}
	if arch == "" {
		arch = runtime.GOARCH
	}

	log.Printf("[hub] 正在为从节点「%s」(%s/%s) 从 GitHub 下载更新包…", wc.name, osName, arch)
	version, assetURL, err := updater.CheckLatestForPlatform(osName, arch)
	if err != nil {
		return fmt.Errorf("获取最新版本失败: %w", err)
	}

	zipData, err := updater.DownloadZip(assetURL)
	if err != nil {
		return fmt.Errorf("下载更新包失败: %w", err)
	}
	log.Printf("[hub] 已下载 v%s 更新包（%d KB），正在推送到从节点「%s」…", version, len(zipData)/1024, wc.name)

	payload, _ := json.Marshal(UpdatePayloadMsg{
		Type:    TypeUpdatePayload,
		Version: version,
		ZipB64:  base64.StdEncoding.EncodeToString(zipData),
	})
	select {
	case wc.send <- payload:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// HubHandler 返回注册到 /internal/ws 的 HTTP 处理函数
func (h *Hub) HubHandler() http.HandlerFunc {
	return h.ServeWS
}

// FormatResult 将 ResultMsg 格式化为可读字符串
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

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
