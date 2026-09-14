package agent

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tools"
)

// Background jobs (PRD §6.3): shell commands that outlived the shell
// tool's wait window (or were started with background: true) whose exit
// lands in the agent's mailbox and wakes it, exactly like a child's answer.
// The shell tool starts every process (internal/proc); the runtime adopts
// it, kills it at its timeout or when stopped, and fires when it exits.
// Internally they are "monitors" of kind "command"; the kind field is kept
// so the log stays readable if other sources return later.

// Monitor is one background job owned by an agent.
type Monitor struct {
	ID      string
	Kind    string // command
	Label   string
	Spec    string
	Started time.Time

	mu       sync.Mutex
	state    protocol.MonitorState
	progress string
	cancel   context.CancelFunc
}

// Info is the protocol view.
func (m *Monitor) Info() protocol.MonitorInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return protocol.MonitorInfo{ID: m.ID, Agent: "", Kind: m.Kind, Label: m.Label, Spec: m.Spec, State: m.state,
		Started: m.Started.UTC().Format(time.RFC3339), Progress: m.progress}
}

// --- agent side ---

// monitors returns the tools.Monitors implementation for this agent.
func (a *Agent) monitorsAPI() tools.Monitors { return monitorsAPI{a: a} }

type monitorsAPI struct{ a *Agent }

func (m monitorsAPI) AdoptCommand(command string, job tools.Job, timeout time.Duration) (string, error) {
	return m.a.adoptMonitor(command, job, timeout)
}
func (m monitorsAPI) List() []tools.MonitorStatus {
	var out []tools.MonitorStatus
	for _, mon := range m.a.monitorList() {
		in := mon.Info()
		out = append(out, tools.MonitorStatus{ID: in.ID, Kind: in.Kind, Label: in.Label, Spec: in.Spec, State: string(in.State), Progress: in.Progress, Started: mon.Started})
	}
	return out
}
func (m monitorsAPI) Stop(id string) error { return m.a.stopMonitor(id, "stopped by agent") }
func (m monitorsAPI) Has(id string) bool {
	m.a.mu.Lock()
	defer m.a.mu.Unlock()
	_, ok := m.a.monitors[id]
	return ok
}

func (a *Agent) monitorList() []*Monitor {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*Monitor, 0, len(a.monitors))
	for _, m := range a.monitors {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// adoptMonitor turns a running shell command into a job: the process keeps
// running under the tool's writer; the monitor waits on it, kills it at the
// timeout (counted from its start) or when stopped, and fires like any job.
func (a *Agent) adoptMonitor(command string, job tools.Job, timeout time.Duration) (string, error) {
	if !a.Alive() {
		return "", fmt.Errorf("agent %s is %s", a.ID, a.StateOf())
	}
	m := &Monitor{ID: NewID("m"), Kind: "command", Spec: command, Started: job.Started(), state: protocol.MonitorRunning}
	m.Label = monitorLabel(command)
	m.progress = fmt.Sprintf("%d lines", job.Lines())
	ctx, cancel := context.WithCancel(a.ctx)
	m.cancel = cancel
	a.mu.Lock()
	a.monitors[m.ID] = m
	a.armed[m.ID] = true
	a.mu.Unlock()
	_, _ = a.record(context.Background(), event.MonitorStarted, event.MonitorStartedPayload{ID: m.ID, Kind: "command", Label: m.Label, Spec: command})
	_, _ = a.record(context.Background(), event.MonitorArmed, event.MonitorPayload{IDs: []string{m.ID}})
	go a.runAdopted(ctx, m, job, timeout)
	return m.ID, nil
}

func (a *Agent) runAdopted(ctx context.Context, m *Monitor, job tools.Job, timeout time.Duration) {
	if timeout <= 0 {
		timeout = time.Hour
	}
	deadline := time.NewTimer(time.Until(job.Started().Add(timeout)))
	defer deadline.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	killed := false
	for {
		select {
		case <-job.Done():
			res := event.MonitorFiredPayload{ID: m.ID, Kind: m.Kind, Label: m.Label, Output: job.Output()}
			summarizeExit(&res, job.Err(), killed, timeout)
			a.fireMonitor(m, res)
			return
		case <-ctx.Done():
			job.Kill()
			a.finishMonitor(m, protocol.MonitorStopped)
			return
		case <-deadline.C:
			killed = true
			job.Kill()
		case <-tick.C:
			m.mu.Lock()
			m.progress = fmt.Sprintf("%d lines", job.Lines())
			m.mu.Unlock()
		}
	}
}

// summarizeExit fills a job result's summary, exit code and error flag
// from how the process ended.
func summarizeExit(res *event.MonitorFiredPayload, err error, killed bool, timeout time.Duration) {
	switch {
	case killed:
		res.Summary = fmt.Sprintf("background command killed after %s timeout", fmtDuration(timeout))
		res.IsError = true
		res.ExitCode = -1
	case err != nil:
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
			res.Summary = fmt.Sprintf("background command exited %d", res.ExitCode)
		} else {
			res.Summary = "background command failed: " + err.Error()
			res.ExitCode = -1
		}
		res.IsError = true
	default:
		res.Summary = "background command finished (exit 0)"
	}
}

// monitorLabel is the command, cut short for lists.
func monitorLabel(spec string) string {
	if len(spec) > 40 {
		return spec[:40] + "…"
	}
	return spec
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

// --- completion ---

// fireMonitor records the result, puts it in the mailbox, and wakes the
// agent if the monitor is armed.
func (a *Agent) fireMonitor(m *Monitor, res event.MonitorFiredPayload) {
	m.mu.Lock()
	if m.state != protocol.MonitorRunning {
		m.mu.Unlock()
		return
	}
	m.state = protocol.MonitorFired
	m.progress = ""
	m.mu.Unlock()
	_, _ = a.record(context.Background(), event.MonitorFired, res)
	a.mu.Lock()
	delete(a.monitors, m.ID)
	a.monDone = append(a.monDone, res)
	wake := a.armed[m.ID]
	delete(a.armed, m.ID)
	if wake {
		a.wakes[m.ID] = true
	}
	a.mu.Unlock()
	if wake {
		a.signal()
	}
}

// finishMonitor ends a monitor without a result.
func (a *Agent) finishMonitor(m *Monitor, state protocol.MonitorState) {
	m.mu.Lock()
	if m.state != protocol.MonitorRunning {
		m.mu.Unlock()
		return
	}
	m.state = state
	m.mu.Unlock()
	a.mu.Lock()
	delete(a.monitors, m.ID)
	delete(a.armed, m.ID)
	a.mu.Unlock()
}

// stopMonitor cancels a running monitor (kills a background command).
func (a *Agent) stopMonitor(id, reason string) error {
	a.mu.Lock()
	m, ok := a.monitors[id]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("no running monitor %q", id)
	}
	m.cancel()
	a.finishMonitor(m, protocol.MonitorStopped)
	_, _ = a.record(context.Background(), event.MonitorStopped, event.MonitorRefPayload{ID: id, Reason: reason})
	return nil
}

// monitorText renders a fired monitor as the mailbox message the model sees.
func monitorText(r event.MonitorFiredPayload) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Job %q (%s): %s", r.Label, r.ID, r.Summary)
	if r.Output != "" {
		out := r.Output
		if len(out) > 32*1024 {
			out = out[:16*1024] + "\n… [truncated] …\n" + out[len(out)-16*1024:]
		}
		sb.WriteString("\n\n" + out)
	}
	return sb.String()
}
