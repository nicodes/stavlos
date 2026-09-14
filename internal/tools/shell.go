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

// shell is the one command tool. It runs the command and waits up to a
// short window for it; a command still running when the window closes
// continues as a background job (the agent runtime adopts the process as a
// monitor), so the model never has to choose between a sync and an async
// tool, and nothing is killed for being slow.
type shellTool struct{}

const (
	defaultShellWaitSeconds  = 15
	maxShellWaitSeconds      = 300
	defaultJobTimeoutSeconds = 3600
	maxJobTimeoutSeconds     = 7200
	shellOutputCap           = 256 * 1024 // a long job keeps this much of its tail in memory
)

func (shellTool) Def() model.ToolDef {
	return model.ToolDef{Name: "shell", Description: "Run a shell command in the working directory and return its combined output. Use it for searching too (grep -rn, rg, find, ls); read-only commands like these are allowed by default. A command still running after the wait window (default 15 seconds) continues as a background job: you get its id and the output so far, and when it exits you are woken with its exit code and output as a new message, between turns, never mid-turn. For servers, watchers and anything you know is slow, set background to true to skip the wait. If nothing more can be done until a job finishes, end your turn.",
		Schema: schema(map[string]any{
			"command":    prop("string", "The command line to run with bash -c"),
			"wait":       prop("integer", "Seconds to wait for the command before it continues as a background job (default 15, max 300)"),
			"background": prop("boolean", "Start it as a background job at once, without waiting"),
			"timeout":    prop("integer", "Seconds before a background job is killed (default 3600, max 7200)"),
		}, "command")}
}

func (shellTool) PolicyArg(in json.RawMessage) string {
	var a struct {
		Command string `json:"command"`
	}
	_ = decode(in, &a)
	return a.Command
}

func (shellTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	var a struct {
		Command    string `json:"command"`
		Wait       int    `json:"wait"`
		Background bool   `json:"background"`
		Timeout    int    `json:"timeout"`
	}
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return errf("empty command")
	}
	if a.Wait <= 0 {
		a.Wait = defaultShellWaitSeconds
	}
	if a.Wait > maxShellWaitSeconds {
		a.Wait = maxShellWaitSeconds
	}
	if a.Timeout <= 0 {
		a.Timeout = defaultJobTimeoutSeconds
	}
	if a.Timeout > maxJobTimeoutSeconds {
		a.Timeout = maxJobTimeoutSeconds
	}
	timeout := time.Duration(a.Timeout) * time.Second
	if a.Background {
		if env.Mon == nil {
			return errf("background jobs are not available to this agent")
		}
		id, err := env.Mon.StartCommand(a.Command, timeout)
		if err != nil {
			return errf("%v", err)
		}
		return Result{Output: fmt.Sprintf("started job %s; you will be woken with its output when it exits (shell_kill %s stops it)", id, id)}
	}

	job, err := startCommand(a.Command, env.Dir, env.Partial)
	if err != nil {
		return errf("%v", err)
	}
	wait := time.NewTimer(time.Duration(a.Wait) * time.Second)
	defer wait.Stop()
	select {
	case <-job.Done():
		return shellResult(job, env.MaxOutput)
	case <-ctx.Done():
		// cancelled by the turn; the loop marks the call cancelled, we return partial output
		job.Kill()
		<-job.Done()
		return Result{Output: clip(job.Output(), env.MaxOutput), IsError: true}
	case <-wait.C:
	}
	if env.Mon == nil { // no job runtime (tests, restricted agents): keep waiting, kill at the timeout
		deadline := time.NewTimer(time.Until(job.Started().Add(timeout)))
		defer deadline.Stop()
		select {
		case <-job.Done():
			return shellResult(job, env.MaxOutput)
		case <-ctx.Done():
			job.Kill()
			<-job.Done()
			return Result{Output: clip(job.Output(), env.MaxOutput), IsError: true}
		case <-deadline.C:
			job.Kill()
			<-job.Done()
			return Result{Output: clip(job.Output(), env.MaxOutput) + fmt.Sprintf("\n[killed after %ds timeout]", a.Timeout), IsError: true}
		}
	}
	id, err := env.Mon.AdoptCommand(a.Command, job, timeout)
	if err != nil {
		job.Kill()
		<-job.Done()
		return errf("%v", err)
	}
	msg := fmt.Sprintf("still running after %ds; continuing as job %s. You will be woken with its exit code and output when it exits (shell_kill %s stops it).", a.Wait, id, id)
	if out := clip(job.Output(), env.MaxOutput); strings.TrimSpace(out) != "" {
		msg += "\n\noutput so far:\n" + out
	}
	return Result{Output: msg}
}

// shellResult is the inline result of a command that exited in time.
func shellResult(job *runningCommand, maxOutput int) Result {
	text := clip(job.Output(), maxOutput)
	if err := job.Err(); err != nil {
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

// runningCommand is a command started by the shell tool. It implements Job
// so the agent runtime can adopt it when it outlives the wait window.
type runningCommand struct {
	cmd     *exec.Cmd
	out     *partialWriter
	exit    chan struct{} // closed once the process has exited and its output is drained
	err     error         // cmd.Wait's result, valid after exit is closed
	started time.Time
}

// startCommand runs command under bash in its own process group so a kill
// takes its children too. It is not bound to a context: an adopted job
// outlives the tool call that started it.
func startCommand(command, dir string, partial func(string)) (*runningCommand, error) {
	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second // a child that exits but leaves the pipes open does not hang Wait
	out := &partialWriter{fn: partial}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	j := &runningCommand{cmd: cmd, out: out, exit: make(chan struct{}), started: time.Now()}
	go func() {
		j.err = cmd.Wait()
		close(j.exit)
	}()
	return j, nil
}

func (j *runningCommand) Done() <-chan struct{} { return j.exit }
func (j *runningCommand) Err() error            { return j.err }
func (j *runningCommand) Output() string        { return j.out.String() }
func (j *runningCommand) Lines() int            { return j.out.Lines() }
func (j *runningCommand) Started() time.Time    { return j.started }

// Kill ends the whole process group.
func (j *runningCommand) Kill() {
	if j.cmd.Process != nil {
		_ = syscall.Kill(-j.cmd.Process.Pid, syscall.SIGKILL)
	}
}

// partialWriter tees output to env.Partial and a tail-capped buffer.
type partialWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	lines int
	fn    func(string)
}

func (w *partialWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf.Write(p)
	w.lines += bytes.Count(p, []byte("\n"))
	if w.buf.Len() > shellOutputCap {
		b := w.buf.Bytes()
		keep := append([]byte("… [earlier output dropped] …\n"), b[len(b)-shellOutputCap/2:]...)
		w.buf.Reset()
		w.buf.Write(keep)
	}
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

func (w *partialWriter) Lines() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lines
}

// --- shell_kill: stop a background job ---

type shellKillTool struct{}

func (shellKillTool) Def() model.ToolDef {
	return model.ToolDef{Name: "shell_kill", Description: "Stop a background job started by shell. Use it for servers and watchers you no longer need.",
		Schema: schema(map[string]any{"id": prop("string", "The job id shell returned")}, "id")}
}

func (shellKillTool) PolicyArg(in json.RawMessage) string { return idArg(in) }

func (shellKillTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	if env.Mon == nil {
		return errf("background jobs are not available to this agent")
	}
	id := idArg(in)
	if !env.Mon.Has(id) {
		return errf("no running job %q", id)
	}
	if err := env.Mon.Stop(id); err != nil {
		return errf("%v", err)
	}
	return Result{Output: "stopped job " + id}
}
