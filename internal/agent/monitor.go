package agent

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tools"
)

// General monitors (PRD §6.3): sources other than children whose completion
// lands in the agent's mailbox and, when armed (the default), wakes it.
//
//   - command: a shell command run in the background; fires on exit
//   - watch:   a path (file or directory, optional glob); fires once on change
//   - timer:   a duration; fires when it elapses
//
// Children are not monitors: they are agents, in the subagent flow.

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
func (m *Monitor) Info(armed bool) protocol.MonitorInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return protocol.MonitorInfo{ID: m.ID, Agent: "", Kind: m.Kind, Label: m.Label, Spec: m.Spec, State: m.state,
		Started: m.Started.UTC().Format(time.RFC3339), Progress: m.progress, Monitored: armed}
}

// --- agent side ---

// monitors returns the tools.Monitors implementation for this agent.
func (a *Agent) monitorsAPI() tools.Monitors { return monitorsAPI{a: a} }

type monitorsAPI struct{ a *Agent }

func (m monitorsAPI) StartCommand(command string, timeout time.Duration) (string, error) {
	return m.a.startMonitor("command", command, "", 0, timeout)
}
func (m monitorsAPI) StartWatch(path, glob string) (string, error) {
	return m.a.startMonitor("watch", path, glob, 0, 0)
}
func (m monitorsAPI) StartTimer(d time.Duration, note string) (string, error) {
	return m.a.startMonitor("timer", note, "", d.Seconds(), 0)
}
func (m monitorsAPI) List() []tools.MonitorStatus {
	var out []tools.MonitorStatus
	for _, mon := range m.a.monitorList() {
		in := mon.Info(m.a.IsArmed(mon.ID))
		out = append(out, tools.MonitorStatus{ID: in.ID, Kind: in.Kind, Label: in.Label, Spec: in.Spec, State: in.State, Progress: in.Progress, Monitored: in.Monitored, Started: mon.Started})
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
	if kind == "watch" {
		abs := spec
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(a.s.Dir, abs)
		}
		if _, err := os.Stat(abs); err != nil {
			return "", fmt.Errorf("watch: %v", err)
		}
		spec = abs
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
	switch kind {
	case "command":
		s := spec
		if len(s) > 40 {
			s = s[:40] + "…"
		}
		return s
	case "watch":
		l := filepath.Base(spec)
		if glob != "" {
			l += "/" + glob
		}
		return l
	case "timer":
		l := fmtDuration(time.Duration(seconds * float64(time.Second)))
		if spec != "" {
			l += " · " + spec
		}
		return l
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

// runMonitor executes the source and delivers its result.
func (a *Agent) runMonitor(ctx context.Context, m *Monitor, timeout time.Duration) {
	var res event.MonitorFiredPayload
	res.ID, res.Kind, res.Label = m.ID, m.Kind, m.Label
	switch m.Kind {
	case "command":
		res = a.runCommandMonitor(ctx, m, timeout, res)
	case "watch":
		res = runWatchMonitor(ctx, m, res)
	case "timer":
		d := time.Duration(m.Seconds * float64(time.Second))
		remaining := time.Until(m.Started.Add(d))
		if remaining < 0 {
			remaining = 0
		}
		t := time.NewTimer(remaining)
		for {
			select {
			case <-t.C:
				res.Summary = "timer elapsed (" + fmtDuration(d) + ")"
				if m.Spec != "" {
					res.Summary += ": " + m.Spec
				}
				a.fireMonitor(m, res)
				return
			case <-ctx.Done():
				t.Stop()
				a.finishMonitor(m, "stopped")
				return
			case <-time.After(5 * time.Second):
				m.mu.Lock()
				m.progress = fmtDuration(time.Until(m.Started.Add(d))) + " left"
				m.mu.Unlock()
			}
		}
	}
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

// --- watch ---

type fileStamp struct {
	size int64
	mod  int64
}

func snapshot(root, glob string) map[string]fileStamp {
	out := map[string]fileStamp{}
	st, err := os.Stat(root)
	if err != nil {
		return out
	}
	if !st.IsDir() {
		out[root] = fileStamp{st.Size(), st.ModTime().UnixNano()}
		return out
	}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && (d.Name() == ".git" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if glob != "" {
			rel, _ := filepath.Rel(root, p)
			if !policy.Match(glob, rel) && !policy.Match(glob, d.Name()) {
				return nil
			}
		}
		if info, err := d.Info(); err == nil {
			out[p] = fileStamp{info.Size(), info.ModTime().UnixNano()}
		}
		return nil
	})
	return out
}

func runWatchMonitor(ctx context.Context, m *Monitor, res event.MonitorFiredPayload) event.MonitorFiredPayload {
	base := snapshot(m.Spec, m.Glob)
	t := time.NewTicker(watchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return res
		case <-t.C:
		}
		now := snapshot(m.Spec, m.Glob)
		var changed []string
		for p, st := range now {
			if b, ok := base[p]; !ok {
				changed = append(changed, "added "+p)
			} else if b != st {
				changed = append(changed, "modified "+p)
			}
		}
		for p := range base {
			if _, ok := now[p]; !ok {
				changed = append(changed, "removed "+p)
			}
		}
		if len(changed) == 0 {
			continue
		}
		sort.Strings(changed)
		if len(changed) > 20 {
			changed = append(changed[:20], fmt.Sprintf("… and %d more", len(changed)-20))
		}
		res.Summary = fmt.Sprintf("%d change(s) under %s", len(changed), m.Label)
		res.Output = strings.Join(changed, "\n")
		return res
	}
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
	fmt.Fprintf(&sb, "Monitor %q (%s, %s): %s", r.Label, r.Kind, r.ID, r.Summary)
	if r.Output != "" {
		out := r.Output
		if len(out) > 32*1024 {
			out = out[:16*1024] + "\n… [truncated] …\n" + out[len(out)-16*1024:]
		}
		sb.WriteString("\n\n" + out)
	}
	return sb.String()
}
