package agent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tools"
)

// Background jobs (PRD §6.3): shell commands started with bash_async whose
// exit lands in the agent's mailbox and wakes it, exactly like a child's
// finish. Internally they are "monitors" of kind "command"; the kind field
// is kept so the log stays readable if other sources return later.

// Monitor is one general monitor owned by an agent.
type Monitor struct {
	ID      string
	Kind    string // command | watch | timer
	Label   string
	Spec    string
	Glob    string
	Seconds float64
	Started time.Time

	mu       sync.Mutex
	state    string // running | fired | stopped | lost
	progress string
	cancel   context.CancelFunc
	buf      bytes.Buffer // command output (tail-capped)
	lines    int
}

const (
	monitorOutputCap = 256 * 1024
	watchInterval    = time.Second
)

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

func (m monitorsAPI) StartCommand(command string, timeout time.Duration) (string, error) {
	return m.a.startMonitor("command", command, "", 0, timeout)
}
func (m monitorsAPI) List() []tools.MonitorStatus {
	var out []tools.MonitorStatus
	for _, mon := range m.a.monitorList() {
		in := mon.Info()
		out = append(out, tools.MonitorStatus{ID: in.ID, Kind: in.Kind, Label: in.Label, Spec: in.Spec, State: in.State, Progress: in.Progress, Started: mon.Started})
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

// startMonitor creates, logs, arms, and runs a monitor.
func (a *Agent) startMonitor(kind, spec, glob string, seconds float64, timeout time.Duration) (string, error) {
	if !a.Alive() {
		return "", fmt.Errorf("agent %s is %s", a.ID, a.StateOf())
	}
	m := &Monitor{ID: NewID("m"), Kind: kind, Spec: spec, Glob: glob, Seconds: seconds, Started: time.Now(), state: "running"}
	m.Label = monitorLabel(kind, spec, glob, seconds)
	ctx, cancel := context.WithCancel(a.ctx)
	m.cancel = cancel
	a.mu.Lock()
	a.monitors[m.ID] = m
	a.armed[m.ID] = true
	a.mu.Unlock()
	_, _ = a.record(context.Background(), event.MonitorStarted, event.MonitorStartedPayload{ID: m.ID, Kind: kind, Label: m.Label, Spec: spec, Glob: glob, Seconds: seconds})
	_, _ = a.record(context.Background(), event.MonitorArmed, event.MonitorPayload{IDs: []string{m.ID}})
	go a.runMonitor(ctx, m, timeout)
	return m.ID, nil
}

func monitorLabel(kind, spec, glob string, seconds float64) string {
	s := spec
	if len(s) > 40 {
		s = s[:40] + "…"
	}
	return s
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

// runMonitor executes the job and delivers its result.
func (a *Agent) runMonitor(ctx context.Context, m *Monitor, timeout time.Duration) {
	var res event.MonitorFiredPayload
	res.ID, res.Kind, res.Label = m.ID, m.Kind, m.Label
	res = a.runCommandMonitor(ctx, m, timeout, res)
	if ctx.Err() != nil {
		a.finishMonitor(m, "stopped")
		return
	}
	a.fireMonitor(m, res)
}

func (a *Agent) runCommandMonitor(ctx context.Context, m *Monitor, timeout time.Duration, res event.MonitorFiredPayload) event.MonitorFiredPayload {
	if timeout <= 0 {
		timeout = time.Hour
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(tctx, "bash", "-c", m.Spec)
	cmd.Dir = a.s.Dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	w := &monitorWriter{m: m}
	cmd.Stdout, cmd.Stderr = w, w
	err := cmd.Run()
	m.mu.Lock()
	out := m.buf.String()
	m.mu.Unlock()
	res.Output = out
	switch {
	case ctx.Err() != nil:
		res.Summary = "background command stopped"
	case tctx.Err() == context.DeadlineExceeded:
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
	return res
}

type monitorWriter struct{ m *Monitor }

func (w *monitorWriter) Write(p []byte) (int, error) {
	w.m.mu.Lock()
	defer w.m.mu.Unlock()
	w.m.buf.Write(p)
	w.m.lines += bytes.Count(p, []byte("\n"))
	if w.m.buf.Len() > monitorOutputCap {
		b := w.m.buf.Bytes()
		keep := append([]byte("… [earlier output dropped] …\n"), b[len(b)-monitorOutputCap/2:]...)
		w.m.buf.Reset()
		w.m.buf.Write(keep)
	}
	w.m.progress = fmt.Sprintf("%d lines", w.m.lines)
	return len(p), nil
}

// --- completion ---

// fireMonitor records the result, puts it in the mailbox, and wakes the
// agent if the monitor is armed.
func (a *Agent) fireMonitor(m *Monitor, res event.MonitorFiredPayload) {
	m.mu.Lock()
	if m.state != "running" {
		m.mu.Unlock()
		return
	}
	m.state = "fired"
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
func (a *Agent) finishMonitor(m *Monitor, state string) {
	m.mu.Lock()
	if m.state != "running" {
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
	a.finishMonitor(m, "stopped")
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
