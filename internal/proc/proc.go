// Package proc starts the processes agents run — shell commands and MCP
// servers — one way: under bash in their own process group, with a
// scrubbed environment and a tail-capped record of their output. The shell
// tool starts a Job and hands it to the agent runtime when it outlives the
// wait window; the runtime kills, reaps and reports it like any other job.
package proc

import (
	"bytes"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// OutputCap is how much of a long job's output is kept: the tail, once the
// buffer passes it, so the model sees how the command ended.
const OutputCap = 256 * 1024

// Job is a command running (or finished) under bash.
type Job struct {
	cmd     *exec.Cmd
	out     *tail
	exit    chan struct{} // closed once the process has exited and its output is drained
	err     error         // cmd.Wait's result, valid after exit is closed
	started time.Time
}

// Start runs command with bash -c in dir, in its own process group so a
// kill takes its children too, with env as the whole environment (see Env)
// and sink receiving each chunk of output as it arrives (nil for none). It
// is not bound to a context: a job adopted by the runtime outlives the tool
// call that started it.
func Start(command, dir string, env []string, sink func(string)) (*Job, error) {
	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second // a child that exits but leaves the pipes open does not hang Wait
	out := &tail{sink: sink}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	j := &Job{cmd: cmd, out: out, exit: make(chan struct{}), started: time.Now()}
	go func() {
		j.err = cmd.Wait()
		close(j.exit)
	}()
	return j, nil
}

// Done is closed once the process has exited.
func (j *Job) Done() <-chan struct{} { return j.exit }

// Err is the exit error, valid after Done.
func (j *Job) Err() error { return j.err }

// Output is the output so far, tail-capped at OutputCap.
func (j *Job) Output() string { return j.out.String() }

// Lines counts output lines so far.
func (j *Job) Lines() int { return j.out.Lines() }

// Started is when the process started.
func (j *Job) Started() time.Time { return j.started }

// Kill ends the whole process group.
func (j *Job) Kill() {
	if j.cmd.Process != nil {
		_ = syscall.Kill(-j.cmd.Process.Pid, syscall.SIGKILL)
	}
}

// Detach stops streaming output to the sink; the tail is still recorded.
// The runtime calls it when it adopts a job: the tool call that wanted the
// stream is over.
func (j *Job) Detach() { j.out.detach() }

// tail records output up to OutputCap, keeping the tail, and tees each
// chunk to a sink while one is attached.
type tail struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	lines int
	sink  func(string)
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.buf.Write(p)
	t.lines += bytes.Count(p, []byte("\n"))
	if t.buf.Len() > OutputCap {
		b := t.buf.Bytes()
		keep := append([]byte("… [earlier output dropped] …\n"), b[len(b)-OutputCap/2:]...)
		t.buf.Reset()
		t.buf.Write(keep)
	}
	sink := t.sink
	t.mu.Unlock()
	if sink != nil {
		sink(string(p))
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}

func (t *tail) Lines() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lines
}

func (t *tail) detach() {
	t.mu.Lock()
	t.sink = nil
	t.mu.Unlock()
}

// secretName matches environment variable names that usually hold
// credentials. SSH_AUTH_SOCK and the like are not secrets and stay.
var secretName = regexp.MustCompile(`(?i)(API[_-]?KEY|APIKEY|SECRET|TOKEN|PASSWORD|PASSWD|CREDENTIAL|PRIVATE[_-]?KEY)`)

// Env is the daemon's environment for a child process: every variable
// except STAVLOS_* (the harness's own switches) and those whose names look
// like credentials, so a command the model runs cannot read them back. A
// name listed in pass is kept regardless (a token a build legitimately
// needs); extra is appended last and wins.
func Env(pass []string, extra ...string) []string {
	keep := map[string]bool{}
	for _, n := range pass {
		keep[n] = true
	}
	var out []string
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if keep[name] || Passes(name) {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}

// Passes reports whether a variable name survives the scrub on its own.
func Passes(name string) bool {
	return !strings.HasPrefix(name, "STAVLOS_") && !secretName.MatchString(name)
}
