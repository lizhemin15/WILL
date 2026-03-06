package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	ConfigKeyLLMApiKey   = "llm_api_key"
	ConfigKeyLLMBaseURL  = "llm_base_url"
	ConfigKeyLLMModel    = "llm_model"
	ConfigKeyFeishuAppID = "feishu_app_id"
	ConfigKeyFeishuAppSecret = "feishu_app_secret"
	ConfigKeyAllowedOpenIDs  = "allowed_open_ids"
	ConfigKeyMode            = "mode"
	ConfigKeyInternalToken   = "internal_token"
	ConfigKeyWorkerURLs      = "worker_urls"
	ConfigKeyBind            = "bind"
	ConfigKeyPort            = "port"
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
	home, _ := os.UserHomeDir()
	if home != "" {
		return filepath.Join(home, ".will", "will.db")
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
	`)
	return err
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
