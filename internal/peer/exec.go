package peer

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Limits on commands run for a client.
const (
	DefaultExecTimeout = 60 * time.Second
	MaxExecTimeout     = 10 * time.Minute
	MaxExecOutput      = 256 << 10
)

// ExecResult is the outcome of a command.
type ExecResult struct {
	ExitCode   int    `json:"exitCode"`
	Output     string `json:"output"`
	Truncated  bool   `json:"truncated,omitempty"`
	TimedOut   bool   `json:"timedOut,omitempty"`
	DurationMs int64  `json:"durationMs"`
}

// cappedBuffer keeps the first max bytes written to it and notes the rest.
type cappedBuffer struct {
	mu        sync.Mutex
	buf       []byte
	max       int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	room := b.max - len(b.buf)
	if len(p) > room {
		b.truncated = true
		p = p[:max(room, 0)]
	}
	b.buf = append(b.buf, p...)
	// 書き込み側（コマンド）を止めないよう、捨てた分も書けたことにする
	return n, nil
}

// runCommand runs command with `shell -lc` in dir, with stdin closed and
// stdout/stderr combined. On timeout the whole process group is killed.
func runCommand(ctx context.Context, shell, dir, command string, env []string, timeout time.Duration, maxOut int) (ExecResult, error) {
	if shell == "" {
		shell = "/bin/bash"
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out := &cappedBuffer{max: maxOut}
	cmd := exec.Command(shell, "-lc", command)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = out, out
	// 子や孫のプロセスもまとめて止められるよう、別のプロセスグループで動かす
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// バックグラウンドに残ったプロセスが出力を開いたままでも、終了後は長く待たない
	cmd.WaitDelay = time.Second
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return ExecResult{}, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	timedOut := false
	select {
	case err = <-done:
	case <-ctx.Done():
		timedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		err = <-done
	}
	// 他のマシンから実行したコマンドが常駐プロセスを残さないよう、グループごと片付ける
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	res := ExecResult{DurationMs: time.Since(start).Milliseconds(), TimedOut: timedOut}
	out.mu.Lock()
	res.Output, res.Truncated = string(out.buf), out.truncated
	out.mu.Unlock()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.Is(err, exec.ErrWaitDelay):
		// コマンド自体は終わったが、残ったプロセスが出力を開いていた
		res.ExitCode = cmd.ProcessState.ExitCode()
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		return res, err
	}
	return res, nil
}
