// Package peer 的消息协议定义。
// 所有 WebSocket 消息均为 JSON，顶层字段 "type" 区分类型。
package peer

// 消息类型常量
const (
	TypeHello         = "hello"          // worker → main：注册名称
	TypeConfig        = "config"         // main → worker：下发 LLM 配置
	TypeExec          = "exec"           // main → worker：执行命令
	TypeResult        = "result"         // worker → main：命令结果
	TypeUpdatePayload = "update_payload" // main → worker：推送 zip 二进制包（由主节点从 GitHub 下载）
	TypeUpdateResult  = "update_result"  // worker → main：升级结果
)

// HelloMsg worker 连接后第一条消息，携带节点名称及平台信息（用于主节点下载对应架构的更新包）
type HelloMsg struct {
	Type string `json:"type"` // TypeHello
	Name string `json:"name"`
	OS   string `json:"os,omitempty"`   // runtime.GOOS
	Arch string `json:"arch,omitempty"` // runtime.GOARCH
}

// ConfigMsg 主节点向从节点下发 LLM 配置
type ConfigMsg struct {
	Type       string `json:"type"` // TypeConfig
	LLMApiKey  string `json:"llm_api_key"`
	LLMBaseURL string `json:"llm_base_url"`
	LLMModel   string `json:"llm_model"`
}

// ExecMsg 主节点向从节点派发命令
type ExecMsg struct {
	Type       string `json:"type"` // TypeExec
	ID         string `json:"id"`
	Command    string `json:"cmd"`
	WorkDir    string `json:"work_dir"`
	TimeoutSec int    `json:"timeout_sec"`
}

// ResultMsg 从节点执行完毕后返回给主节点（同时被 main.go 通过 FormatResult 使用）
type ResultMsg struct {
	Type     string `json:"type"` // TypeResult
	ID       string `json:"id"`
	OK       bool   `json:"ok"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
}

// UpdatePayloadMsg 主节点向从节点推送完整的 zip 二进制包。
// 由主节点统一从 GitHub 下载，避免从节点因网络问题无法访问 GitHub。
// ZipB64 为 zip 文件的 base64 编码内容。
type UpdatePayloadMsg struct {
	Type    string `json:"type"`    // TypeUpdatePayload
	Version string `json:"version"` // 版本号（仅供展示）
	ZipB64  string `json:"zip_b64"` // base64(zip bytes)
}

// UpdateResultMsg 从节点升级结果（升级成功后进程会退出，此消息可能来不及发送）
type UpdateResultMsg struct {
	Type  string `json:"type"` // TypeUpdateResult
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// WorkerInfo 描述一个已连接的从节点
type WorkerInfo struct {
	Name string // 节点名称（未注册时为空字符串）
}
