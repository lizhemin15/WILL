package internalapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/yourusername/will/internal/exec"
	"github.com/yourusername/will/internal/store"
)

const DefaultExecTimeout = 5 * time.Minute

// ExecRequest 内部 API 请求体
type ExecRequest struct {
	Command string `json:"command"`
	WorkDir string `json:"work_dir"`
	Timeout int    `json:"timeout_sec"` // 0 表示默认
}

// ExecResponse 内部 API 响应
type ExecResponse struct {
	OK       bool   `json:"ok"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
}

// AuthMiddleware 校验 Authorization: Bearer <WILL_INTERNAL_TOKEN>
func AuthMiddleware(token string, next http.HandlerFunc) http.HandlerFunc {
	if token == "" {
		return func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"ok":false,"error":"WILL_INTERNAL_TOKEN not set"}`, http.StatusUnauthorized)
		}
	}
	want := "Bearer " + strings.TrimSpace(token)
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != want {
			http.Error(w, `{"ok":false,"error":"invalid token"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// HandleExec 处理 POST /internal/exec
func HandleExec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"ok":false,"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, ExecResponse{OK: false, Error: "invalid json: " + err.Error()})
		return
	}
	if req.Command == "" {
		writeJSON(w, http.StatusBadRequest, ExecResponse{OK: false, Error: "command required"})
		return
	}

	timeout := DefaultExecTimeout
	if req.Timeout > 0 {
		timeout = time.Duration(req.Timeout) * time.Second
	}
	ctx := r.Context()
	res := exec.Run(ctx, req.Command, req.WorkDir, timeout)

	resp := ExecResponse{
		OK:       res.Err == nil && res.ExitCode == 0,
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		ExitCode: res.ExitCode,
	}
	if res.Err != nil {
		resp.Error = res.Err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ── Pair API ──────────────────────────────────────────────────────────────────

// PairRequest 从节点发起配对请求
type PairRequest struct {
	Token      string `json:"token"`
	WorkerURL  string `json:"worker_url"`  // 保留字段（现已不需要，WebSocket 模式下从节点主动连出）
	WorkerName string `json:"worker_name"` // 从节点自定义名称
}

// PairResponse 主节点返回配对结果，同时携带 LLM 配置供从节点继承
type PairResponse struct {
	OK          bool   `json:"ok"`
	WorkerToken string `json:"worker_token,omitempty"`
	LLMApiKey   string `json:"llm_api_key,omitempty"`
	LLMBaseURL  string `json:"llm_base_url,omitempty"`
	LLMModel    string `json:"llm_model,omitempty"`
	Error       string `json:"error,omitempty"`
}

// HandlePair 处理 POST /internal/pair：校验配对码，注册从节点 URL，返回 internal token
func HandlePair(s *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, PairResponse{OK: false, Error: "method not allowed"})
			return
		}
		var req PairRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, PairResponse{OK: false, Error: "invalid json"})
			return
		}
		if req.Token == "" {
			writeJSON(w, http.StatusBadRequest, PairResponse{OK: false, Error: "token required"})
			return
		}
		if !s.ConsumePairToken(req.Token) {
			writeJSON(w, http.StatusUnauthorized, PairResponse{OK: false, Error: "配对码无效或已过期"})
			return
		}

		// 获取或生成 internal token
		internalToken, ok := s.GetConfig(store.ConfigKeyInternalToken)
		if !ok || internalToken == "" {
			b := make([]byte, 16)
			_, _ = rand.Read(b)
			internalToken = hex.EncodeToString(b)
			_ = s.SetConfig(store.ConfigKeyInternalToken, internalToken)
		}

		// 注册从节点 URL
		if req.WorkerURL != "" {
			existing, _ := s.GetConfig(store.ConfigKeyWorkerURLs)
			var urls []string
			for _, u := range strings.Split(existing, ",") {
				u = strings.TrimSpace(u)
				if u != "" && u != req.WorkerURL {
					urls = append(urls, u)
				}
			}
			urls = append(urls, req.WorkerURL)
			_ = s.SetConfig(store.ConfigKeyWorkerURLs, strings.Join(urls, ","))
		}

		// 将主节点 LLM 配置一并返回，从节点无需单独配置
		llmKey, _ := s.GetConfig(store.ConfigKeyLLMApiKey)
		llmBase, _ := s.GetConfig(store.ConfigKeyLLMBaseURL)
		llmModel, _ := s.GetConfig(store.ConfigKeyLLMModel)

		writeJSON(w, http.StatusOK, PairResponse{
			OK:          true,
			WorkerToken: internalToken,
			LLMApiKey:   llmKey,
			LLMBaseURL:  llmBase,
			LLMModel:    llmModel,
		})
	}
}
