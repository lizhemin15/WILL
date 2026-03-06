package internalapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/yourusername/will/internal/exec"
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
