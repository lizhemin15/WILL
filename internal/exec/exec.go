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

// Run 在本地执行系统命令。timeout 为 0 表示不限制。
func Run(ctx context.Context, command string, workDir string, timeout time.Duration) Result {
	cmdLine := parseCommand(command)
	if len(cmdLine) == 0 {
		return Result{ExitCode: -1, Err: fmt.Errorf("empty command")}
	}

	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	var cmd *exec.Cmd
	if len(cmdLine) == 1 {
		cmd = exec.CommandContext(ctx, cmdLine[0])
	} else {
		cmd = exec.CommandContext(ctx, cmdLine[0], cmdLine[1:]...)
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

func parseCommand(command string) []string {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil
	}
	if runtime.GOOS == "windows" {
		return parseCommandWindows(command)
	}
	return parseCommandUnix(command)
}

func parseCommandUnix(s string) []string {
	var parts []string
	var b strings.Builder
	quote := rune(0)
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				b.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
		case r == ' ' || r == '\t':
			if b.Len() > 0 {
				parts = append(parts, b.String())
				b.Reset()
			}
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		parts = append(parts, b.String())
	}
	return parts
}

func parseCommandWindows(s string) []string {
	return strings.Fields(s)
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
