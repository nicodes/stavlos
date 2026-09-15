package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/policy"
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
	if sub := ts["shell"].Subject(json.RawMessage(`{"command":"git push"}`)); sub.Kind != policy.KindCommand || sub.Primary() != "git push" {
		t.Fatalf("subject %+v", sub)
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
	specs   []string
}

func (f *fakeMonitors) AdoptCommand(command string, job Job, timeout time.Duration) (string, error) {
	f.adopted = append(f.adopted, job)
	f.specs = append(f.specs, command)
	return fmt.Sprintf("m%d", len(f.adopted)), nil
}
func (f *fakeMonitors) Stop(id string) error { return nil }
func (f *fakeMonitors) Has(id string) bool   { return false }

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
	if r.IsError || !strings.HasPrefix(r.Output, "still running after 1s; continuing as job m1") || !strings.Contains(r.Output, "output so far:\nearly\n") || time.Since(start) > 1400*time.Millisecond {
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
	// background: started and adopted at once, no wait, no stream
	var streamed strings.Builder
	env.Partial = func(s string) { streamed.WriteString(s) }
	start = time.Now()
	r = sh.Run(ctx, json.RawMessage(`{"command":"echo bg; sleep 30","background":true}`), env)
	if r.IsError || !strings.Contains(r.Output, "started job m2") || len(mon.adopted) != 2 || mon.specs[1] != "echo bg; sleep 30" || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("background: %+v adopted=%d", r, len(mon.adopted))
	}
	bg := mon.adopted[1]
	for bg.Lines() < 1 {
		time.Sleep(10 * time.Millisecond)
	}
	if bg.Output() != "bg\n" || streamed.Len() != 0 {
		t.Fatalf("background output %q streamed %q", bg.Output(), streamed.String())
	}
	bg.Kill()
	<-bg.Done()
	env.Partial = nil
	// kill ends the process group
	r = sh.Run(ctx, json.RawMessage(`{"command":"sleep 30 & wait","wait":1}`), env)
	job = mon.adopted[2]
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
				f.items[i].Status = event.TodoStatus(status)
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

// TestResolvePath: relative paths hang off the root, .. escapes, symlinks
// resolve on the existing part, and nothing is expanded.
func TestResolvePath(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	os.WriteFile(filepath.Join(other, "secret.txt"), []byte("s"), 0o644)
	os.Symlink(other, filepath.Join(root, "link"))
	os.Symlink(filepath.Join(other, "secret.txt"), filepath.Join(root, "file-link"))
	rootReal, _ := filepath.EvalSymlinks(root)
	otherReal, _ := filepath.EvalSymlinks(other)
	cases := map[string]string{
		"":                    rootReal,
		"a/b.txt":             filepath.Join(rootReal, "a", "b.txt"),
		"./a/../b.txt":        filepath.Join(rootReal, "b.txt"),
		"../x":                filepath.Join(filepath.Dir(rootReal), "x"),
		"/etc/hostname":       "/etc/hostname",
		"link/secret.txt":     filepath.Join(otherReal, "secret.txt"),
		"link/new/deeper.txt": filepath.Join(otherReal, "new", "deeper.txt"),
		"file-link":           filepath.Join(otherReal, "secret.txt"),
		"~/x":                 filepath.Join(rootReal, "~", "x"),
		"$HOME/x":             filepath.Join(rootReal, "$HOME", "x"),
	}
	for in, want := range cases {
		if got := ResolvePath(root, in); got != want {
			t.Errorf("ResolvePath(%q) = %q want %q", in, got, want)
		}
	}
	// read opens what ResolvePath says: the link's target
	r := Builtin()["read"].Run(context.Background(), json.RawMessage(`{"path":"link/secret.txt"}`), &Env{Dir: root})
	if r.IsError || !strings.Contains(r.Output, "s") {
		t.Fatalf("%+v", r)
	}
}

// TestReadStopsAtTheBudget: read never builds more than the output budget
// (plus one line) in memory, and says how to continue.
func TestReadStopsAtTheBudget(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	for i := 0; i < 20000; i++ {
		sb.WriteString("line with some text on it to take up space\n")
	}
	os.WriteFile(filepath.Join(dir, "big.txt"), []byte(sb.String()), 0o644)
	r := Builtin()["read"].Run(context.Background(), json.RawMessage(`{"path":"big.txt","limit":1000000}`), &Env{Dir: dir, MaxOutput: 8 * 1024})
	if r.IsError || len(r.Output) > 9*1024 || !strings.Contains(r.Output, "continue with offset=") {
		t.Fatalf("len %d err %v tail %q", len(r.Output), r.IsError, r.Output[max(0, len(r.Output)-80):])
	}
	// Truncation never splits a character.
	s := strings.Repeat("é", 100)
	for _, n := range []int{1, 2, 3, 50, 51} {
		if c := cutRunes(s, n); !utf8.ValidString(c) || len(c) > n {
			t.Errorf("cutRunes(%d) = %q", n, c)
		}
		if c := tailRunes(s, n); !utf8.ValidString(c) || len(c) > n {
			t.Errorf("tailRunes(%d) = %q", n, c)
		}
	}
	if c := clip(strings.Repeat("é", 1000), 301); !utf8.ValidString(c) {
		t.Errorf("clip split a rune: %q", c[:10])
	}
}

func TestRecipient(t *testing.T) {
	for in, want := range map[string]string{
		"user": "user", "@User": "user", " human ": "user", "@HUMAN": "user",
		"scout": "scout", "@scout-2": "scout-2", "0192ab": "0192ab",
	} {
		if got := Recipient(in); got != want {
			t.Errorf("Recipient(%q) = %q, want %q", in, got, want)
		}
	}
}
