package exec

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error
}

// Run 通过系统 shell 执行命令字符串，支持管道、重定向、&&、|| 等全部 shell 语法。
// Unix 使用 sh -c，Windows 使用 cmd /c。timeout 为 0 表示不限制。
func Run(ctx context.Context, command string, workDir string, timeout time.Duration) Result {
	command = strings.TrimSpace(command)
	if command == "" {
		return Result{ExitCode: -1, Err: fmt.Errorf("empty command")}
	}

	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/c", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	if workDir != "" {
		cmd.Dir = workDir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	out := Result{
		Stdout:   strings.TrimSpace(stdout.String()),
		Stderr:   strings.TrimSpace(stderr.String()),
		ExitCode: -1,
		Err:      err,
	}
	if cmd.ProcessState != nil {
		out.ExitCode = cmd.ProcessState.ExitCode()
	}
	return out
}

func (r Result) String() string {
	if r.Err != nil {
		return fmt.Sprintf("exit=%d err=%v\nstdout:\n%s\nstderr:\n%s", r.ExitCode, r.Err, r.Stdout, r.Stderr)
	}
	if r.Stderr != "" {
		return fmt.Sprintf("exit=%d\nstdout:\n%s\nstderr:\n%s", r.ExitCode, r.Stdout, r.Stderr)
	}
	return fmt.Sprintf("exit=%d\n%s", r.ExitCode, r.Stdout)
}
