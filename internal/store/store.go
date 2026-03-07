package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	ConfigKeyLLMApiKey              = "llm_api_key"
	ConfigKeyLLMBaseURL             = "llm_base_url"
	ConfigKeyLLMModel               = "llm_model"
	ConfigKeyFeishuAppID            = "feishu_app_id"
	ConfigKeyFeishuAppSecret        = "feishu_app_secret"
	ConfigKeyAllowedOpenIDs         = "allowed_open_ids"
	ConfigKeyMode                   = "mode"
	ConfigKeyInternalToken          = "internal_token"
	ConfigKeyWorkerURLs             = "worker_urls"
	ConfigKeyBind                   = "bind"
	ConfigKeyPort                   = "port"
	ConfigKeyUpdateCheckAt          = "update_check_at"
	ConfigKeyLatestVersion          = "latest_version"
	ConfigKeyUpdatePromptAt         = "update_prompt_at"
	ConfigKeyUpdateNotifyOpenID     = "update_notify_open_id"
	ConfigKeyPostUpdateNotifyOpenID = "post_update_notify_open_id"
	ConfigKeyFeishuSubscribeMode    = "feishu_subscribe_mode"
	ConfigKeyTimezone               = "timezone"
	ConfigKeyMainURL                = "main_url"
	ConfigKeyWorkerName             = "worker_name"

	KindUserScheduled   = "user_scheduled"
	KindDoVersionCheck  = "do_version_check"
	KindRemindUpdate    = "remind_update"

	keepConversationRows = 50
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

func (s *Store) Close() error { return s.db.Close() }

func defaultDBPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".will", "will.db")
}

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS config (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS memory (scope TEXT NOT NULL, key TEXT NOT NULL, value TEXT NOT NULL, PRIMARY KEY (scope, key))`,
		`CREATE TABLE IF NOT EXISTS todos (id INTEGER PRIMARY KEY AUTOINCREMENT, open_id TEXT NOT NULL, title TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', created_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS scheduled_tasks (id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, payload TEXT NOT NULL DEFAULT '', run_at INTEGER NOT NULL, created_at INTEGER NOT NULL, open_id TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS conversation (id INTEGER PRIMARY KEY AUTOINCREMENT, open_id TEXT NOT NULL, role TEXT NOT NULL, content TEXT NOT NULL, created_at INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_todos_open_id ON todos(open_id)`,
		`CREATE INDEX IF NOT EXISTS idx_sched_run_at ON scheduled_tasks(run_at)`,
		`CREATE INDEX IF NOT EXISTS idx_conv_open_id ON conversation(open_id, created_at)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	// 幂等地添加 feishu_task_id 列（已存在时忽略错误）
	_, _ = s.db.Exec(`ALTER TABLE todos ADD COLUMN feishu_task_id TEXT NOT NULL DEFAULT ''`)
	return nil
}

// ── Config ────────────────────────────────────────────────────────────────────

func (s *Store) GetConfig(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var v string
	err := s.db.QueryRow("SELECT value FROM config WHERE key=?", key).Scan(&v)
	if err != nil {
		return "", false
	}
	return v, true
}

func (s *Store) SetConfig(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("INSERT OR REPLACE INTO config(key,value) VALUES(?,?)", key, value)
	return err
}

func (s *Store) GetAllowedOpenIDs() []string {
	v, ok := s.GetConfig(ConfigKeyAllowedOpenIDs)
	if !ok || v == "" {
		return nil
	}
	var ids []string
	for _, id := range strings.Split(v, ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func (s *Store) AddAllowedOpenID(openID string) error {
	existing := s.GetAllowedOpenIDs()
	for _, id := range existing {
		if id == openID {
			return nil
		}
	}
	existing = append(existing, openID)
	return s.SetConfig(ConfigKeyAllowedOpenIDs, strings.Join(existing, ","))
}

// ── Memory ────────────────────────────────────────────────────────────────────

func (s *Store) SetMemory(scope, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("INSERT OR REPLACE INTO memory(scope,key,value) VALUES(?,?,?)", scope, key, value)
	return err
}

func (s *Store) GetMemory(scope, key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var v string
	err := s.db.QueryRow("SELECT value FROM memory WHERE scope=? AND key=?", scope, key).Scan(&v)
	if err != nil {
		return "", false
	}
	return v, true
}

func (s *Store) DeleteMemory(scope, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("DELETE FROM memory WHERE scope=? AND key=?", scope, key)
	return err
}

func (s *Store) ListMemory(scope string) (map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query("SELECT key, value FROM memory WHERE scope=?", scope)
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

// ── Scheduled Tasks ───────────────────────────────────────────────────────────

type ScheduledTaskDue struct {
	ID      int64
	Kind    string
	Payload string
	OpenID  string
	RunAt   int64
}

func (s *Store) AddScheduledTask(kind, payload string, runAt int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec("INSERT INTO scheduled_tasks(kind,payload,run_at,created_at) VALUES(?,?,?,?)",
		kind, payload, runAt, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func (s *Store) ListScheduledTasksDue(now int64) ([]ScheduledTaskDue, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query("SELECT id,kind,payload,COALESCE(open_id,''),run_at FROM scheduled_tasks WHERE run_at<=? ORDER BY run_at", now)
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
	_, err := s.db.Exec("DELETE FROM scheduled_tasks WHERE id=?", id)
	return err
}

// UserScheduledPayload is the JSON payload for user-defined scheduled tasks.
type UserScheduledPayload struct {
	Instruction string `json:"instruction"`
	CronExpr    string `json:"cron_expr"`
}

type UserScheduledTask struct {
	ID          int64
	Instruction string
	CronExpr    string
	RunAt       int64
}

func (s *Store) AddUserScheduledTask(openID, instruction, cronExpr string, nextRun int64) (int64, error) {
	p := UserScheduledPayload{Instruction: instruction, CronExpr: cronExpr}
	payload, _ := json.Marshal(p)
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		"INSERT INTO scheduled_tasks(kind,payload,run_at,created_at,open_id) VALUES(?,?,?,?,?)",
		KindUserScheduled, string(payload), nextRun, time.Now().Unix(), openID,
	)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func (s *Store) ListUserScheduledTasks(openID string) ([]UserScheduledTask, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(
		"SELECT id,payload,run_at FROM scheduled_tasks WHERE kind=? AND open_id=? ORDER BY run_at",
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
		out = append(out, UserScheduledTask{ID: id, Instruction: p.Instruction, CronExpr: p.CronExpr, RunAt: runAt})
	}
	return out, rows.Err()
}

func (s *Store) GetUserScheduledTaskByID(id int64, openID string) (*UserScheduledTask, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var payload string
	var runAt int64
	err := s.db.QueryRow(
		"SELECT payload, run_at FROM scheduled_tasks WHERE id=? AND kind=? AND open_id=?",
		id, KindUserScheduled, openID,
	).Scan(&payload, &runAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p UserScheduledPayload
	_ = json.Unmarshal([]byte(payload), &p)
	return &UserScheduledTask{ID: id, Instruction: p.Instruction, CronExpr: p.CronExpr, RunAt: runAt}, nil
}

func (s *Store) DeleteUserScheduledTask(id int64, openID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec("DELETE FROM scheduled_tasks WHERE id=? AND kind=? AND open_id=?", id, KindUserScheduled, openID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) UpdateUserScheduledTask(id int64, openID, instruction, cronExpr string, nextRun int64) (bool, error) {
	p := UserScheduledPayload{Instruction: instruction, CronExpr: cronExpr}
	payload, _ := json.Marshal(p)
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		"UPDATE scheduled_tasks SET payload=?, run_at=? WHERE id=? AND kind=? AND open_id=?",
		string(payload), nextRun, id, KindUserScheduled, openID,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ── Conversation History ──────────────────────────────────────────────────────

type ConvMessage struct {
	Role    string
	Content string
}

func (s *Store) AppendConversation(openID, role, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("INSERT INTO conversation(open_id,role,content,created_at) VALUES(?,?,?,?)",
		openID, role, content, time.Now().Unix())
	if err != nil {
		return err
	}
	// trim old rows
	_, _ = s.db.Exec(`DELETE FROM conversation WHERE open_id=? AND id NOT IN (
		SELECT id FROM conversation WHERE open_id=? ORDER BY created_at DESC LIMIT ?)`,
		openID, openID, keepConversationRows)
	return nil
}

// ClearConversation 清空指定用户的全部对话历史
func (s *Store) ClearConversation(openID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("DELETE FROM conversation WHERE open_id=?", openID)
	return err
}

// ConversationCount 返回指定用户当前的对话消息条数
func (s *Store) ConversationCount(openID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM conversation WHERE open_id=?", openID).Scan(&n)
	return n
}

// ── Todos (本地存储，不依赖飞书 Task API) ─────────────────────────────────────

// TodoItem 代表一条本地待办
type TodoItem struct {
	ID    int64
	Title string
	Done  bool
}

// AddTodo 新增一条待办，返回自增 ID
func (s *Store) AddTodo(openID, title string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		"INSERT INTO todos(open_id,title,status,created_at) VALUES(?,?,?,?)",
		openID, title, "pending", time.Now().Unix(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListTodos 返回指定用户的所有待办（未完成在前）
func (s *Store) ListTodos(openID string) ([]TodoItem, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(
		"SELECT id,title,status FROM todos WHERE open_id=? ORDER BY status ASC, created_at ASC",
		openID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TodoItem
	for rows.Next() {
		var t TodoItem
		var status string
		if err := rows.Scan(&t.ID, &t.Title, &status); err != nil {
			return nil, err
		}
		t.Done = status == "done"
		out = append(out, t)
	}
	return out, rows.Err()
}

// CompleteTodo 将指定待办标记为完成
func (s *Store) CompleteTodo(id int64, openID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("UPDATE todos SET status='done' WHERE id=? AND open_id=?", id, openID)
	return err
}

// DeleteTodo 删除指定待办
func (s *Store) DeleteTodo(id int64, openID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("DELETE FROM todos WHERE id=? AND open_id=?", id, openID)
	return err
}

// UpdateTodoTitle 修改待办标题
func (s *Store) UpdateTodoTitle(id int64, openID, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("UPDATE todos SET title=? WHERE id=? AND open_id=?", title, id, openID)
	return err
}

// ── Pair Tokens ───────────────────────────────────────────────────────────────

// SavePairToken 保存一个短期有效的配对令牌（scope=system:pair，key=token，value=expiry unix）
func (s *Store) SavePairToken(token string, expiresAt int64) error {
	return s.SetMemory("system:pair", token, strconv.FormatInt(expiresAt, 10))
}

// ConsumePairToken 校验令牌是否有效：有效则删除并返回 true，无效/过期返回 false
func (s *Store) ConsumePairToken(token string) bool {
	val, ok := s.GetMemory("system:pair", token)
	if !ok || val == "" {
		return false
	}
	expiry, err := strconv.ParseInt(val, 10, 64)
	if err != nil || time.Now().Unix() > expiry {
		_ = s.DeleteMemory("system:pair", token)
		return false
	}
	_ = s.DeleteMemory("system:pair", token)
	return true
}

func (s *Store) GetRecentConversation(openID string, maxMessages int) ([]ConvMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(
		`SELECT role, content FROM (
			SELECT role, content, created_at FROM conversation WHERE open_id=?
			ORDER BY created_at DESC LIMIT ?
		) ORDER BY created_at ASC`,
		openID, maxMessages,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConvMessage
	for rows.Next() {
		var m ConvMessage
		if err := rows.Scan(&m.Role, &m.Content); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
