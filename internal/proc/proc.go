// Package proc starts the processes agents run — shell commands and MCP
// servers — one way: under bash in their own process group, with a
// scrubbed environment and a tail-capped record of their output. The shell
// tool starts a Job and hands it to the agent runtime when it outlives the
// wait window; the runtime kills, reaps and reports it like any other job.
package proc

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nicodes/stavlos/internal/sandbox"
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
// kill takes its children too, with env as the whole environment (see Env),
// inside sb (nil for no sandbox), and sink receiving each chunk of output as
// it arrives (nil for none). It
// is not bound to a context: a job adopted by the runtime outlives the tool
// call that started it.
func Start(command, dir string, env []string, sink func(string), sb *sandbox.Spec) (*Job, error) {
	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if sb != nil {
		if _, err := sandbox.Wrap(cmd, *sb); err != nil {
			return nil, fmt.Errorf("sandbox: %w", err)
		}
	}
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

// A child process gets an allowlist of the daemon's environment: what
// programs need to find themselves and behave (the path, the home, the
// locale, toolchain settings). Everything else stays out, whatever it is
// called: a credential with an unusual name, a D-Bus address (systemd-run
// would start a process outside every restriction), an ssh-agent socket
// (signing with the user's keys), a display. Config env.pass adds names.

// passNames are variables children keep by name.
var passNames = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "TERM": true, "COLORTERM": true, "TERM_PROGRAM": true,
	"LANG": true, "LANGUAGE": true, "TZ": true, "TMPDIR": true, "PWD": true, "HOSTNAME": true, "SHLVL": true,
	"EDITOR": true, "VISUAL": true, "PAGER": true, "LESS": true, "NO_COLOR": true, "FORCE_COLOR": true, "CI": true,
	"CC": true, "CXX": true, "CFLAGS": true, "CXXFLAGS": true, "CPPFLAGS": true, "LDFLAGS": true, "LD_LIBRARY_PATH": true, "MAKEFLAGS": true,
	"JAVA_HOME": true, "VIRTUAL_ENV": true,
	"GIT_AUTHOR_NAME": true, "GIT_AUTHOR_EMAIL": true, "GIT_COMMITTER_NAME": true, "GIT_COMMITTER_EMAIL": true,
}

// passPrefixes are families of toolchain variables children keep.
var passPrefixes = []string{
	"LC_", "XDG_CONFIG_", "XDG_DATA_", "XDG_CACHE_HOME", "XDG_STATE_HOME",
	"GO", "CARGO_", "RUSTUP_", "RUSTC", "NODE_", "NPM_CONFIG_", "PNPM_", "YARN_", "BUN_", "DENO_",
	"PYTHON", "PIP_", "UV_", "POETRY_", "CONDA_", "PYENV_", "MISE_", "ASDF_", "NVM_", "VOLTA_",
	"GRADLE_", "MAVEN_", "ANDROID_", "DOTNET_", "PKG_CONFIG",
}

// secretName matches variable names that usually hold credentials; such a
// variable is dropped even when its family is allowed (GOOGLE_API_KEY).
var secretName = regexp.MustCompile(`(?i)(API[_-]?KEY|APIKEY|SECRET|TOKEN|PASSWORD|PASSWD|CREDENTIAL|PRIVATE[_-]?KEY|(^|_)AUTH($|_))`)

// Env is the environment for a child process: the daemon's variables that
// pass (see Passes), every name listed in pass (config env.pass, for a
// token a build legitimately needs), then extra, which wins.
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
		if keep[name] || Passes(name) && !urlCredential(kv[len(name)+1:]) {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}

// urlCredential reports a value that carries a user and password in a URL
// (GOPROXY=https://user:token@proxy/, PIP_INDEX_URL, UV_INDEX_URL): a
// credential the name does not give away. env.pass still lets it through.
var urlCredential = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://[^/@\s]+:[^/@\s]+@`).MatchString

// Passes reports whether a variable reaches child processes on its own:
// allowlisted, not the harness's own (STAVLOS_*), and not named like a
// credential.
func Passes(name string) bool {
	if strings.HasPrefix(name, "STAVLOS_") || secretName.MatchString(name) {
		return false
	}
	if passNames[name] {
		return true
	}
	for _, p := range passPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
