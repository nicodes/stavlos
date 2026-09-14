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
)

func TestFileToolsAndBash(t *testing.T) {
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
		t.Fatal("grep should be gone: bash covers it")
	}
	if _, ok := ts["glob"]; ok {
		t.Fatal("glob should be gone: bash covers it")
	}
	var partial strings.Builder
	env.Partial = func(s string) { partial.WriteString(s) }
	r = ts["bash"].Run(ctx, json.RawMessage(`{"command":"echo hi; exit 3"}`), env)
	if !r.IsError || !strings.Contains(r.Output, "hi") || !strings.Contains(r.Output, "exit status 3") || partial.String() != "hi\n" {
		t.Fatalf("%+v partial=%q", r, partial.String())
	}
	// cancellation kills the process group and returns partial output
	cctx, cancel := context.WithCancel(ctx)
	start := time.Now()
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	r = ts["bash"].Run(cctx, json.RawMessage(`{"command":"echo start; sleep 30; echo never"}`), env)
	if time.Since(start) > 5*time.Second || !strings.Contains(r.Output, "start") || strings.Contains(r.Output, "never") {
		t.Fatalf("cancel: %+v after %v", r, time.Since(start))
	}
	if ts["bash"].PolicyArg(json.RawMessage(`{"command":"git push"}`)) != "git push" {
		t.Fatal("policy arg")
	}
	_ = os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
}

func TestBashDefaultTimeout(t *testing.T) {
	if defaultBashTimeoutSeconds != 3*60 {
		t.Fatalf("default bash timeout = %ds, want 180s", defaultBashTimeoutSeconds)
	}
	if !strings.Contains(string(bashTool{}.Def().Schema), "default 180, max 1800") {
		t.Fatalf("bash schema does not advertise the default: %s", bashTool{}.Def().Schema)
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
