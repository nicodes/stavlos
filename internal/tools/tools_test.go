package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
)

func TestFileToolsAndShell(t *testing.T) {
	dir := t.TempDir()
	env := &Env{Dir: dir}
	ts := Builtin()
	ctx := context.Background()
	r := ts["apply_patch"].Run(ctx, json.RawMessage(`{"patch":"*** Begin Patch\n*** Add File: a/b.txt\n+hello\n+world\n*** End Patch"}`), env)
	if r.IsError {
		t.Fatal(r.Output)
	}
	r = ts["apply_patch"].Run(ctx, json.RawMessage(`{"patch":"*** Begin Patch\n*** Update File: a/b.txt\n hello\n-world\n+there\n*** End Patch"}`), env)
	if r.IsError {
		t.Fatal(r.Output)
	}
	for _, gone := range []string{"write", "edit"} {
		if _, ok := ts[gone]; ok {
			t.Fatalf("%s should be gone: apply_patch covers it", gone)
		}
	}
	r = ts["read"].Run(ctx, json.RawMessage(`{"path":"a/b.txt"}`), env)
	if !strings.Contains(r.Output, "2\tthere") {
		t.Fatal(r.Output)
	}
	if _, ok := ts["grep"]; ok {
		t.Fatal("grep should be gone: shell covers it")
	}
	if _, ok := ts["glob"]; ok {
		t.Fatal("glob should be gone: shell covers it")
	}
	var partial strings.Builder
	env.Partial = func(s string) { partial.WriteString(s) }
	r = ts["shell"].Run(ctx, json.RawMessage(`{"command":"echo hi; exit 3"}`), env)
	if !r.IsError || !strings.Contains(r.Output, "hi") || !strings.Contains(r.Output, "exit status 3") || partial.String() != "hi\n" {
		t.Fatalf("%+v partial=%q", r, partial.String())
	}
	// cancellation kills the process group and returns partial output
	cctx, cancel := context.WithCancel(ctx)
	start := time.Now()
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	r = ts["shell"].Run(cctx, json.RawMessage(`{"command":"echo start; sleep 30; echo never"}`), env)
	if time.Since(start) > 5*time.Second || !strings.Contains(r.Output, "start") || strings.Contains(r.Output, "never") {
		t.Fatalf("cancel: %+v after %v", r, time.Since(start))
	}
	if ts["shell"].PolicyArg(json.RawMessage(`{"command":"git push"}`)) != "git push" {
		t.Fatal("policy arg")
	}
	for _, gone := range []string{"bash", "bash_async", "bash_async_kill"} {
		if _, ok := ts[gone]; ok {
			t.Fatalf("%s should be gone: shell covers it", gone)
		}
	}
	_ = os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
}

// fakeMonitors records jobs handed over by the shell tool.
type fakeMonitors struct {
	adopted []Job
	started []string
}

func (f *fakeMonitors) StartCommand(command string, timeout time.Duration) (string, error) {
	f.started = append(f.started, command)
	return "m-bg", nil
}
func (f *fakeMonitors) AdoptCommand(command string, job Job, timeout time.Duration) (string, error) {
	f.adopted = append(f.adopted, job)
	return "m-adopted", nil
}
func (f *fakeMonitors) List() []MonitorStatus { return nil }
func (f *fakeMonitors) Stop(id string) error  { return nil }
func (f *fakeMonitors) Has(id string) bool    { return false }

// TestShellWaitWindow: a command that exits inside the window returns
// inline; one that outlives it is handed to the monitors with the output so
// far, keeps running, and reports its exit through the Job; background:
// true skips the wait; without a job runtime the tool waits it out.
func TestShellWaitWindow(t *testing.T) {
	ctx := context.Background()
	mon := &fakeMonitors{}
	env := &Env{Dir: t.TempDir(), Mon: mon}
	sh := Builtin()["shell"]
	if !strings.Contains(string(sh.Def().Schema), "default 15, max 300") || defaultShellWaitSeconds != 15 {
		t.Fatalf("shell schema does not advertise the wait window: %s", sh.Def().Schema)
	}
	r := sh.Run(ctx, json.RawMessage(`{"command":"echo fast","wait":5}`), env)
	if r.IsError || r.Output != "fast\n" || len(mon.adopted) != 0 {
		t.Fatalf("inline: %+v adopted=%d", r, len(mon.adopted))
	}
	start := time.Now()
	r = sh.Run(ctx, json.RawMessage(`{"command":"echo early; sleep 1.5; echo late; exit 4","wait":1}`), env)
	if r.IsError || !strings.HasPrefix(r.Output, "still running after 1s; continuing as job m-adopted") || !strings.Contains(r.Output, "output so far:\nearly\n") || time.Since(start) > 1400*time.Millisecond {
		t.Fatalf("handover: %+v after %v", r, time.Since(start))
	}
	if len(mon.adopted) != 1 {
		t.Fatalf("adopted %d", len(mon.adopted))
	}
	job := mon.adopted[0]
	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the job should have exited")
	}
	if job.Output() != "early\nlate\n" || job.Lines() != 2 || job.Err() == nil || !strings.Contains(job.Err().Error(), "4") {
		t.Fatalf("job: out=%q lines=%d err=%v", job.Output(), job.Lines(), job.Err())
	}
	r = sh.Run(ctx, json.RawMessage(`{"command":"sleep 30","background":true}`), env)
	if r.IsError || !strings.Contains(r.Output, "started job m-bg") || len(mon.started) != 1 || mon.started[0] != "sleep 30" {
		t.Fatalf("background: %+v started=%v", r, mon.started)
	}
	// kill ends the process group
	r = sh.Run(ctx, json.RawMessage(`{"command":"sleep 30 & wait","wait":1}`), env)
	job = mon.adopted[1]
	job.Kill()
	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("kill should end the job")
	}
	// no runtime: the tool waits past the window and returns inline
	r = sh.Run(ctx, json.RawMessage(`{"command":"sleep 1.2; echo waited","wait":1}`), &Env{Dir: env.Dir})
	if r.IsError || r.Output != "waited\n" {
		t.Fatalf("no runtime: %+v", r)
	}
	r = sh.Run(ctx, json.RawMessage(`{"command":"sleep 30","background":true}`), &Env{Dir: env.Dir})
	if !r.IsError || !strings.Contains(r.Output, "not available") {
		t.Fatalf("background without a runtime: %+v", r)
	}
}

// fakeTodos is an in-memory tools.Todos.
type fakeTodos struct{ items []event.TodoItem }

func (f *fakeTodos) Add(text string) (string, error) {
	id := "t" + string(rune('0'+len(f.items)+1))
	f.items = append(f.items, event.TodoItem{ID: id, Text: text, Status: "pending"})
	return id, nil
}
func (f *fakeTodos) Update(id, status, text string) error {
	for i := range f.items {
		if f.items[i].ID == id {
			if status != "" {
				f.items[i].Status = status
			}
			if text != "" {
				f.items[i].Text = text
			}
			return nil
		}
	}
	return os.ErrNotExist
}
func (f *fakeTodos) List() []event.TodoItem { return f.items }

func TestTodoTools(t *testing.T) {
	ts := Builtin()
	ctx := context.Background()
	// unavailable without a list (the preset has no "todo")
	if r := ts["todo_add"].Run(ctx, json.RawMessage(`{"text":"x"}`), &Env{}); !r.IsError || !strings.Contains(r.Output, "not available") {
		t.Fatalf("no list: %+v", r)
	}
	f := &fakeTodos{}
	env := &Env{Todo: f}
	r := ts["todo_add"].Run(ctx, json.RawMessage(`{"text":"  Run the tests "}`), env)
	if r.IsError || r.Output != "added t1: Run the tests" || len(f.items) != 1 {
		t.Fatalf("add: %+v %+v", r, f.items)
	}
	if r := ts["todo_add"].Run(ctx, json.RawMessage(`{"text":"  "}`), env); !r.IsError {
		t.Fatalf("empty text should fail: %+v", r)
	}
	if r := ts["todo_update"].Run(ctx, json.RawMessage(`{"id":"t1","status":"doing"}`), env); !r.IsError || !strings.Contains(r.Output, "pending, in_progress, done, cancelled") {
		t.Fatalf("bad status: %+v", r)
	}
	if r := ts["todo_update"].Run(ctx, json.RawMessage(`{"id":"t1"}`), env); !r.IsError {
		t.Fatalf("nothing to change should fail: %+v", r)
	}
	r = ts["todo_update"].Run(ctx, json.RawMessage(`{"id":"t1","status":"in_progress","text":"Run all the tests"}`), env)
	if r.IsError || r.Output != "updated t1 → in_progress" || f.items[0].Status != "in_progress" || f.items[0].Text != "Run all the tests" {
		t.Fatalf("update: %+v %+v", r, f.items)
	}
	if r := ts["todo_update"].Run(ctx, json.RawMessage(`{"id":"t9","status":"done"}`), env); !r.IsError {
		t.Fatalf("unknown id should fail: %+v", r)
	}
}

func TestAskUserNeedsOptions(t *testing.T) {
	ts := Builtin()
	r := ts["ask_user"].Run(context.Background(), json.RawMessage(`{"questions":[{"question":"q?"}]}`), &Env{Ask: fakeAsker{}})
	if !r.IsError || !strings.Contains(r.Output, "at least one option") {
		t.Fatalf("options are required: %+v", r)
	}
	r = ts["ask_user"].Run(context.Background(), json.RawMessage(`{"questions":[{"question":"q?","options":[{"label":"a"},{"label":"b"}]}]}`), &Env{Ask: fakeAsker{}})
	if r.IsError || r.Output != "q? → a, b, typed" {
		t.Fatalf("answers: %+v", r)
	}
}

type fakeAsker struct{}

func (fakeAsker) Ask(ctx context.Context, qs []protocol.Question) ([]string, error) {
	return []string{"a, b, typed"}, nil
}
