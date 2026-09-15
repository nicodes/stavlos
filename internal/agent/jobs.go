package agent

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tools"
)

// Background jobs (PRD §6.3): shell commands that outlived the shell tool's
// wait window (or were started with background: true). The shell tool
// starts every process (internal/proc); the runtime adopts it, kills it at
// its timeout or when stopped, and when it exits logs its result and puts
// it in the agent's inbox, which wakes the agent between turns.

// jobRun is a running job's process, which the log cannot hold.
type jobRun struct {
	id, command string
	handle      tools.Job
	cancel      context.CancelFunc
}

// jobsAPI is the tools.Jobs of one agent.
type jobsAPI struct{ a *Agent }

func (j jobsAPI) AdoptCommand(command string, job tools.Job, timeout time.Duration) (string, error) {
	return j.a.adoptJob(command, job, timeout)
}

func (j jobsAPI) Stop(id string) error { return j.a.stopJob(id, "stopped by agent") }

func (j jobsAPI) Has(id string) bool {
	j.a.c.mu.Lock()
	defer j.a.c.mu.Unlock()
	_, ok := j.a.jobs[id]
	return ok
}

// adoptJob takes over a running command as a background job.
func (a *Agent) adoptJob(command string, job tools.Job, timeout time.Duration) (string, error) {
	s := a.c
	s.mu.Lock()
	if a.state().killed {
		s.mu.Unlock()
		return "", fmt.Errorf("agent %s is killed", a.ID)
	}
	id := NewID("m")
	if _, err := s.commitLocked(context.Background(), s.event(a.ID, event.JobStarted, event.JobStartedPayload{ID: id, Command: command})); err != nil {
		s.mu.Unlock()
		return "", err
	}
	ctx, cancel := context.WithCancel(a.ctx)
	run := &jobRun{id: id, command: command, handle: job, cancel: cancel}
	a.jobs[id] = run
	s.wg.Add(1)
	s.mu.Unlock()
	go a.watchJob(ctx, run, timeout)
	return id, nil
}

// watchJob waits for a job: it kills it at the timeout (counted from its
// start) or when its context ends, and reports how it exited.
func (a *Agent) watchJob(ctx context.Context, run *jobRun, timeout time.Duration) {
	defer a.c.wg.Done()
	if timeout <= 0 {
		timeout = time.Hour
	}
	deadline := time.NewTimer(time.Until(run.handle.Started().Add(timeout)))
	defer deadline.Stop()
	killed := false
	for {
		select {
		case <-run.handle.Done():
			res := event.JobFinishedPayload{ID: run.id, Output: run.handle.Output()}
			summarizeExit(&res, run.handle.Err(), killed, timeout)
			a.finishJob(run, res)
			return
		case <-ctx.Done():
			run.handle.Kill()
			<-run.handle.Done()
			return
		case <-deadline.C:
			killed = true
			run.handle.Kill()
		}
	}
}

// finishJob logs a job's result with its input in one transaction, unless
// the job was stopped meanwhile.
func (a *Agent) finishJob(run *jobRun, res event.JobFinishedPayload) {
	s := a.c
	s.mu.Lock()
	if a.jobs[run.id] != run {
		s.mu.Unlock()
		return
	}
	delete(a.jobs, run.id)
	wake, _ := s.commitLocked(context.Background(),
		s.event(a.ID, event.JobFinished, res),
		s.event(a.ID, event.InputQueued, event.Input{ID: NewID("i"), Kind: event.InputJob, Job: run.id}))
	s.mu.Unlock()
	signal(wake)
}

// stopJob kills a running job and logs that it was stopped.
func (a *Agent) stopJob(id, reason string) error {
	s := a.c
	s.mu.Lock()
	run, ok := a.jobs[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("no running job %q", id)
	}
	delete(a.jobs, id)
	_, err := s.commitLocked(context.Background(), s.event(a.ID, event.JobStopped, event.JobStoppedPayload{ID: id, Reason: reason}))
	s.mu.Unlock()
	run.cancel()
	return err
}

// jobInfosLocked describes the running jobs, oldest first.
func (a *Agent) jobInfosLocked() []protocol.JobInfo {
	runs := make([]*jobRun, 0, len(a.jobs))
	for _, r := range a.jobs {
		runs = append(runs, r)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].handle.Started().Before(runs[j].handle.Started()) })
	var out []protocol.JobInfo
	for _, r := range runs {
		out = append(out, protocol.JobInfo{ID: r.id, Agent: a.ID, Label: jobLabel(r.command), Spec: r.command,
			Started: r.handle.Started().UTC().Format(time.RFC3339), Progress: fmt.Sprintf("%d lines", r.handle.Lines())})
	}
	return out
}

// summarizeExit fills a job result's summary, exit code and error flag.
func summarizeExit(res *event.JobFinishedPayload, err error, killed bool, timeout time.Duration) {
	switch {
	case killed:
		res.Summary = fmt.Sprintf("background command killed after %s timeout", fmtDuration(timeout))
		res.IsError, res.ExitCode = true, -1
	case err != nil:
		res.IsError, res.ExitCode = true, -1
		res.Summary = "background command failed: " + err.Error()
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
			res.Summary = fmt.Sprintf("background command exited %d", res.ExitCode)
		}
	default:
		res.Summary = "background command finished (exit 0)"
	}
}

// jobLabel is the command, cut short for lists.
func jobLabel(command string) string {
	if r := []rune(command); len(r) > 40 {
		return string(r[:40]) + "…"
	}
	return command
}

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}
