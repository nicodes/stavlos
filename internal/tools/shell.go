package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/nicodes/stavlos/internal/clip"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/proc"
	"github.com/nicodes/stavlos/internal/toolname"
)

// shellTool is the one command tool. It runs the command and waits up to a
// short window for it; a command still running when the window closes
// continues as a background job (the agent runtime adopts the process as a
// job), so the model never has to choose between a sync and an async
// tool, and nothing is killed for being slow.
type shellTool struct{}

const (
	defaultShellWaitSeconds  = 15
	maxShellWaitSeconds      = 300
	defaultJobTimeoutSeconds = 3600
	maxJobTimeoutSeconds     = 7200
)

func (shellTool) Def() model.ToolDef {
	return model.ToolDef{Name: toolname.Shell, Description: "Run a shell command in the working directory and return its combined output. Use it to build, test and run things; find and search files with glob and grep instead, which never need the human's approval inside the working directories. A command still running after the wait window (default 15 seconds) continues as a background job: you get its id and the output so far, and when it exits you are woken with its exit code and output as a new message, between turns, never mid-turn. For servers, watchers and anything you know is slow, set background to true to skip the wait. If nothing more can be done until a job finishes, end your turn.",
		Schema: schemaOf(shellInput{})}
}

type shellInput struct {
	Command    string `json:"command" desc:"The command line to run with bash -c" req:"true"`
	Wait       int    `json:"wait" desc:"Seconds to wait for the command before it continues as a background job (default 15, max 300)"`
	Background bool   `json:"background" desc:"Start it as a background job at once, without waiting"`
	Timeout    int    `json:"timeout" desc:"Seconds before a background job is killed (default 3600, max 7200)"`
}

func (shellTool) Subject(in json.RawMessage) policy.Subject {
	var a shellInput
	_ = decode(in, &a)
	return policy.Command(a.Command)
}

func (shellTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	var a shellInput
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
	if a.Background && env.Jobs == nil {
		return errf("background jobs are not available to this agent")
	}
	job, err := proc.Start(a.Command, env.Dir, proc.Env(env.PassEnv), env.Partial, env.Sandbox)
	if err != nil {
		return errf("%v", err)
	}
	if a.Background { // no wait: the runtime takes it at once
		job.Detach()
		id, err := env.Jobs.AdoptCommand(a.Command, job, timeout)
		if err != nil {
			job.Kill()
			<-job.Done()
			return errf("%v", err)
		}
		return Result{Output: fmt.Sprintf("started job %s; you will be woken with its output when it exits (shell_kill %s stops it)", id, id)}
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
		return Result{Output: clip.Middle(job.Output(), env.MaxOutput), IsError: true}
	case <-wait.C:
	}
	if env.Jobs == nil { // no job runtime (tests, restricted agents): keep waiting, kill at the timeout
		deadline := time.NewTimer(time.Until(job.Started().Add(timeout)))
		defer deadline.Stop()
		select {
		case <-job.Done():
			return shellResult(job, env.MaxOutput)
		case <-ctx.Done():
			job.Kill()
			<-job.Done()
			return Result{Output: clip.Middle(job.Output(), env.MaxOutput), IsError: true}
		case <-deadline.C:
			job.Kill()
			<-job.Done()
			return Result{Output: clip.Middle(job.Output(), env.MaxOutput) + fmt.Sprintf("\n[killed after %ds timeout]", a.Timeout), IsError: true}
		}
	}
	job.Detach() // the call that wanted the stream is over
	id, err := env.Jobs.AdoptCommand(a.Command, job, timeout)
	if err != nil {
		job.Kill()
		<-job.Done()
		return errf("%v", err)
	}
	msg := fmt.Sprintf("still running after %ds; continuing as job %s. You will be woken with its exit code and output when it exits (shell_kill %s stops it).", a.Wait, id, id)
	if out := clip.Middle(job.Output(), env.MaxOutput); strings.TrimSpace(out) != "" {
		msg += "\n\noutput so far:\n" + out
	}
	return Result{Output: msg}
}

// shellResult is the inline result of a command that exited in time.
func shellResult(job *proc.Job, maxOutput int) Result {
	text := clip.Middle(job.Output(), maxOutput)
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

// --- shell_kill: stop a background job ---

type shellKillTool struct{}

func (shellKillTool) Def() model.ToolDef {
	return model.ToolDef{Name: toolname.ShellKill, Description: "Stop a background job started by shell. Use it for servers and watchers you no longer need.",
		Schema: schemaOf(shellKillInput{})}
}

type shellKillInput struct {
	ID string `json:"id" desc:"The job id shell returned" req:"true"`
}

func (shellKillTool) Subject(in json.RawMessage) policy.Subject { return policy.ID(idArg(in)) }

func (shellKillTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	if env.Jobs == nil {
		return errf("background jobs are not available to this agent")
	}
	id := idArg(in)
	if !env.Jobs.Has(id) {
		return errf("no running job %q", id)
	}
	if err := env.Jobs.Stop(id); err != nil {
		return errf("%v", err)
	}
	return Result{Output: "stopped job " + id}
}
