package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nicodes/stavlos/internal/model"
)

type bashTool struct{}

func (bashTool) Def() model.ToolDef {
	return model.ToolDef{Name: "bash", Description: "Run a shell command in the working directory and return its combined output. Long-running commands are killed at the timeout.",
		Schema: schema(map[string]any{
			"command": prop("string", "The command line to run with bash -c"),
			"timeout": prop("integer", "Seconds before the command is killed (default 300, max 1800)"),
		}, "command")}
}

func (bashTool) PolicyArg(in json.RawMessage) string {
	var a struct {
		Command string `json:"command"`
	}
	_ = decode(in, &a)
	return a.Command
}

// partialWriter tees output to env.Partial and a buffer.
type partialWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
	fn  func(string)
}

func (w *partialWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf.Write(p)
	w.mu.Unlock()
	if w.fn != nil {
		w.fn(string(p))
	}
	return len(p), nil
}

func (w *partialWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (bashTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	var a struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return errf("empty command")
	}
	if a.Timeout <= 0 {
		a.Timeout = 300
	}
	if a.Timeout > 1800 {
		a.Timeout = 1800
	}
	tctx, cancel := context.WithTimeout(ctx, time.Duration(a.Timeout)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(tctx, "bash", "-c", a.Command)
	cmd.Dir = env.Dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { // kill the whole process group
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
	out := &partialWriter{fn: env.Partial}
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	text := clip(out.String(), env.MaxOutput)
	switch {
	case ctx.Err() != nil:
		// cancelled by the turn; the loop marks the call cancelled, we return partial output
		return Result{Output: text, IsError: true}
	case tctx.Err() == context.DeadlineExceeded:
		return Result{Output: text + fmt.Sprintf("\n[killed after %ds timeout]", a.Timeout), IsError: true}
	case err != nil:
		if ee, ok := err.(*exec.ExitError); ok {
			return Result{Output: text + fmt.Sprintf("\n[exit status %d]", ee.ExitCode()), IsError: true}
		}
		return Result{Output: text + "\n[error: " + err.Error() + "]", IsError: true}
	}
	if strings.TrimSpace(text) == "" {
		text = "(no output)"
	}
	return Result{Output: text}
}
