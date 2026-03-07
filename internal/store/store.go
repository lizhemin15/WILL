package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	ConfigKeyLLMApiKey        = "llm_api_key"
	ConfigKeyLLMBaseURL       = "llm_base_url"
	ConfigKeyLLMModel         = "llm_model"
	ConfigKeyLLMSearchModel   = "llm_search_model" // 涉及搜索最新信息时临时切换的模型
	ConfigKeyFeishuAppID      = "feishu_app_id"
	ConfigKeyFeishuAppSecret  = "feishu_app_secret"
	ConfigKeyAllowedOpenIDs   = "allowed_open_ids"
	ConfigKeyMode             = "mode"
	ConfigKeyInternalToken    = "internal_token"
	ConfigKeyWorkerURLs       = "worker_urls"
	ConfigKeyBind             = "bind"
	ConfigKeyPort             = "port"
	ConfigKeyUpdateCheckAt    = "update_check_at"
	ConfigKeyLatestVersion    = "latest_version"
	ConfigKeyUpdatePromptAt   = "update_prompt_at"
	ConfigKeyUpdateNotifyOpenID   = "update_notify_open_id"
	ConfigKeyPostUpdateNotifyOpenID = "post_update_notify_open_id" // 更新重启后向该用户推送新版本说明
	ConfigKeyFeishuSubscribeMode = "feishu_subscribe_mode"         // "webhook" | "ws"
)

type Store struct {
	db   *sql.DB
	path string
	mu   sync.RWMutex
}

func Open(path string) (*Store, error) {
	if path == "" {
		path = defaultDBPath()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func defaultDBPath() string {
	if p := os.Getenv("WILL_DB_PATH"); p != "" {
		return p
	}
	return "./will.db"
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS config (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL DEFAULT '',
			updated_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS memory (
			scope TEXT NOT NULL,
			key TEXT NOT NULL,
			value TEXT NOT NULL DEFAULT '',
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (scope, key)
		);
		CREATE TABLE IF NOT EXISTS scheduled_tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			kind TEXT NOT NULL,
			payload TEXT NOT NULL DEFAULT '{}',
			run_at INTEGER NOT NULL,
			created_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_scheduled_tasks_run_at ON scheduled_tasks(run_at);
		CREATE TABLE IF NOT EXISTS todos (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			open_id TEXT NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'pending',
			created_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_todos_open_id ON todos(open_id);
		CREATE TABLE IF NOT EXISTS conversation (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			open_id TEXT NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_conversation_open_id ON conversation(open_id);
	`)
	if err != nil {
		return err
	}
	_, _ = s.db.Exec("ALTER TABLE scheduled_tasks ADD COLUMN open_id TEXT")
	return nil
}

func (s *Store) GetConfig(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var v string
	err := s.db.QueryRow("SELECT value FROM config WHERE key = ?", key).Scan(&v)
	return v, err == nil
}

func (s *Store) SetConfig(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		"INSERT INTO config (key, value, updated_at) VALUES (?, ?, ?) ON CONFLICT(key) DO UPDATE SET value = ?, updated_at = ?",
		key, value, time.Now().Unix(), value, time.Now().Unix(),
	)
	return err
}

func (s *Store) GetMemory(scope, key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var v string
	err := s.db.QueryRow("SELECT value FROM memory WHERE scope = ? AND key = ?", scope, key).Scan(&v)
	return v, err == nil
}

func (s *Store) SetMemory(scope, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	_, err := s.db.Exec(
		"INSERT INTO memory (scope, key, value, updated_at) VALUES (?, ?, ?, ?) ON CONFLICT(scope, key) DO UPDATE SET value = ?, updated_at = ?",
		scope, key, value, now, value, now,
	)
	return err
}

func (s *Store) ListMemory(scope string) (map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query("SELECT key, value FROM memory WHERE scope = ?", scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (s *Store) GetAllowedOpenIDs() []string {
	v, ok := s.GetConfig(ConfigKeyAllowedOpenIDs)
	if !ok || v == "" {
		return nil
	}
	var ids []string
	for _, id := range strings.Split(v, ",") {
		id = strings.TrimSpace(id)
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func (s *Store) AddAllowedOpenID(openID string) error {
	ids := s.GetAllowedOpenIDs()
	for _, id := range ids {
		if id == openID {
			return nil
		}
	}
	ids = append(ids, openID)
	return s.SetConfig(ConfigKeyAllowedOpenIDs, strings.Join(ids, ","))
}

func (s *Store) Close() error {
	return s.db.Close()
}

// Scheduled task
func (s *Store) AddScheduledTask(kind, payload string, runAt int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		"INSERT INTO scheduled_tasks (kind, payload, run_at, created_at) VALUES (?, ?, ?, ?)",
		kind, payload, runAt, time.Now().Unix(),
	)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// ScheduledTaskDue 到期任务（含 open_id、run_at 供 user_scheduled 使用）
type ScheduledTaskDue struct {
	ID      int64
	Kind    string
	Payload string
	OpenID  string
	RunAt   int64
}

func (s *Store) ListScheduledTasksDue(now int64) ([]ScheduledTaskDue, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query("SELECT id, kind, payload, COALESCE(open_id,''), run_at FROM scheduled_tasks WHERE run_at <= ? ORDER BY run_at", now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScheduledTaskDue
	for rows.Next() {
		var t ScheduledTaskDue
		if err := rows.Scan(&t.ID, &t.Kind, &t.Payload, &t.OpenID, &t.RunAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteScheduledTask(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("DELETE FROM scheduled_tasks WHERE id = ?", id)
	return err
}

// UserScheduledPayload 用户定时任务 payload
type UserScheduledPayload struct {
	Instruction string `json:"instruction"`
	Repeat     string `json:"repeat"` // "daily" | ""
}

const KindUserScheduled = "user_scheduled"

// AddUserScheduledTask 添加用户定时任务，返回任务 id
func (s *Store) AddUserScheduledTask(openID, instruction string, runAt int64, repeat string) (int64, error) {
	payload := UserScheduledPayload{Instruction: instruction, Repeat: repeat}
	payloadBytes, _ := json.Marshal(payload)
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		"INSERT INTO scheduled_tasks (kind, payload, run_at, created_at, open_id) VALUES (?, ?, ?, ?, ?)",
		KindUserScheduled, string(payloadBytes), runAt, time.Now().Unix(), openID,
	)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// UserScheduledTask 用户定时任务列表项
type UserScheduledTask struct {
	ID          int64
	Instruction string
	Repeat      string
	RunAt       int64
}

func (s *Store) ListUserScheduledTasks(openID string) ([]UserScheduledTask, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(
		"SELECT id, payload, run_at FROM scheduled_tasks WHERE kind = ? AND open_id = ? ORDER BY run_at",
		KindUserScheduled, openID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserScheduledTask
	for rows.Next() {
		var id int64
		var payload string
		var runAt int64
		if err := rows.Scan(&id, &payload, &runAt); err != nil {
			return nil, err
		}
		var p UserScheduledPayload
		_ = json.Unmarshal([]byte(payload), &p)
		out = append(out, UserScheduledTask{ID: id, Instruction: p.Instruction, Repeat: p.Repeat, RunAt: runAt})
	}
	return out, rows.Err()
}

// DeleteUserScheduledTask 删除用户定时任务（仅限本人）
func (s *Store) DeleteUserScheduledTask(id int64, openID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec("DELETE FROM scheduled_tasks WHERE id = ? AND kind = ? AND open_id = ?", id, KindUserScheduled, openID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// UpdateUserScheduledTask 更新用户定时任务（instruction / run_at / repeat）
func (s *Store) UpdateUserScheduledTask(id int64, openID string, instruction string, runAt int64, repeat string) (bool, error) {
	payload := UserScheduledPayload{Instruction: instruction, Repeat: repeat}
	payloadBytes, _ := json.Marshal(payload)
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		"UPDATE scheduled_tasks SET payload = ?, run_at = ? WHERE id = ? AND kind = ? AND open_id = ?",
		string(payloadBytes), runAt, id, KindUserScheduled, openID,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Todo 待办项
type Todo struct {
	ID        int64
	OpenID    string
	Title     string
	Status    string // "pending" | "done"
	CreatedAt int64
}

func (s *Store) AddTodo(openID, title string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	title = strings.TrimSpace(title)
	if title == "" {
		return 0, nil
	}
	res, err := s.db.Exec(
		"INSERT INTO todos (open_id, title, status, created_at) VALUES (?, ?, 'pending', ?)",
		openID, title, time.Now().Unix(),
	)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func (s *Store) ListTodos(openID string) ([]Todo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(
		"SELECT id, open_id, title, status, created_at FROM todos WHERE open_id = ? ORDER BY status ASC, id ASC",
		openID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Todo
	for rows.Next() {
		var t Todo
		if err := rows.Scan(&t.ID, &t.OpenID, &t.Title, &t.Status, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) SetTodoStatus(id int64, openID, status string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		"UPDATE todos SET status = ? WHERE id = ? AND open_id = ?",
		status, id, openID,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) DeleteTodo(id int64, openID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec("DELETE FROM todos WHERE id = ? AND open_id = ?", id, openID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ConvMessage 单条对话（user 或 assistant）
type ConvMessage struct {
	Role    string
	Content string
}

const keepConversationRows = 50 // 每用户最多保留条数，超出删最旧的

// AppendConversation 追加一条对话；每用户仅保留最近 keepConversationRows 条
func (s *Store) AppendConversation(openID, role, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	_, err := s.db.Exec(
		"INSERT INTO conversation (open_id, role, content, created_at) VALUES (?, ?, ?, ?)",
		openID, role, content, now,
	)
	if err != nil {
		return err
	}
	var count int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM conversation WHERE open_id = ?", openID).Scan(&count)
	if count > keepConversationRows {
		toDelete := count - keepConversationRows
		_, _ = s.db.Exec(
			"DELETE FROM conversation WHERE open_id = ? AND id IN (SELECT id FROM conversation WHERE open_id = ? ORDER BY id ASC LIMIT ?)",
			openID, openID, toDelete,
		)
	}
	return nil
}

// GetRecentConversation 按时间正序返回该用户最近 maxMessages 条对话（用于拼进 LLM 上下文）
func (s *Store) GetRecentConversation(openID string, maxMessages int) ([]ConvMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(
		"SELECT role, content FROM conversation WHERE open_id = ? ORDER BY id DESC LIMIT ?",
		openID, maxMessages,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []ConvMessage
	for rows.Next() {
		var m ConvMessage
		if err := rows.Scan(&m.Role, &m.Content); err != nil {
			return nil, err
		}
		list = append(list, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 倒序成时间正序（最旧在前）
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}
	return list, nil
}
