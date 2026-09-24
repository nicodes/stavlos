package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nicodes/stavlos/internal/clip"
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
	if !strings.Contains(r.Output, "there") || strings.Contains(r.Output, "2\t") {
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

// fakeJobs records jobs handed over by the shell tool.
type fakeJobs struct {
	adopted []Job
	specs   []string
}

func (f *fakeJobs) AdoptCommand(command string, job Job, timeout time.Duration) (string, error) {
	f.adopted = append(f.adopted, job)
	f.specs = append(f.specs, command)
	return fmt.Sprintf("m%d", len(f.adopted)), nil
}
func (f *fakeJobs) Stop(id string) error { return nil }
func (f *fakeJobs) Has(id string) bool   { return false }

// TestShellWaitWindow: a command that exits inside the window returns
// inline; one that outlives it is handed to the agent's jobs with the output so
// far, keeps running, and reports its exit through the Job; background:
// true skips the wait; without a job runtime the tool waits it out.
func TestShellWaitWindow(t *testing.T) {
	ctx := context.Background()
	mon := &fakeJobs{}
	env := &Env{Dir: t.TempDir(), Jobs: mon}
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

// fakeTodos is a todo list in memory.
type fakeTodos struct{ items []event.TodoItem }

func (f *fakeTodos) Edit(add []TodoAdd, updates []TodoUpdate) ([]event.TodoItem, error) {
	items := append([]event.TodoItem(nil), f.items...)
	for _, u := range updates {
		found := false
		for i := range items {
			if items[i].ID == u.ID {
				found = true
				if u.Status != "" {
					items[i].Status = event.TodoStatus(u.Status)
				}
				if u.Text != "" {
					items[i].Text = u.Text
				}
			}
		}
		if !found {
			return nil, os.ErrNotExist
		}
	}
	for _, it := range add {
		status := event.TodoStatus(it.Status)
		if status == "" {
			status = event.TodoPending
		}
		items = append(items, event.TodoItem{ID: "t" + strconv.Itoa(len(items)+1), Text: it.Text, Status: status})
	}
	f.items = items
	return items, nil
}

func (f *fakeTodos) List() []event.TodoItem { return f.items }

// TestTodoTool: one call adds steps, updates items by id, or both, and
// returns the whole list; bad input changes nothing.
func TestTodoTool(t *testing.T) {
	ts := Builtin()
	ctx := context.Background()
	run := func(env *Env, in string) Result { return ts["todo"].Run(ctx, json.RawMessage(in), env) }
	if r := run(&Env{}, `{"add":[{"text":"x"}]}`); !r.IsError || !strings.Contains(r.Output, "not available") {
		t.Fatalf("no list: %+v", r)
	}
	f := &fakeTodos{}
	env := &Env{Todo: f}
	if r := run(env, `{"add":[{"text":"  Run the tests "},{"text":"Fix the bug"}]}`); r.IsError || len(f.items) != 2 || f.items[0].Text != "Run the tests" || !strings.Contains(r.Output, "added t2 [pending] Fix the bug") || !strings.Contains(r.Output, "0 of 2 done") {
		t.Fatalf("add: %+v %+v", r, f.items)
	}
	for in, want := range map[string]string{
		`{}`:                      "nothing to do",
		`{"add":[{"text":"  "}]}`: "needs text",
		`{"add":[{"text":"x","status":"doing"}]}`:   "pending, in_progress, done, cancelled",
		`{"update":[{"id":"t1","status":"doing"}]}`: "pending, in_progress, done, cancelled",
		`{"update":[{"id":"t1"}]}`:                  "nothing to change",
		`{"update":[{"status":"done"}]}`:            "needs the item's id",
		`{"update":[{"id":"t9","status":"done"}]}`:  "exist",
	} {
		if r := run(env, in); !r.IsError || !strings.Contains(r.Output, want) || len(f.items) != 2 {
			t.Fatalf("%s: want an error with %q, got %+v, list %+v", in, want, r, f.items)
		}
	}
	r := run(env, `{"update":[{"id":"t1","status":"in_progress","text":"Run all the tests"}],"add":[{"text":"Ship it"}]}`)
	if r.IsError || f.items[0].Status != "in_progress" || f.items[0].Text != "Run all the tests" || len(f.items) != 3 || !strings.Contains(r.Output, "added t3 [pending] Ship it") || !strings.Contains(r.Output, "t1 → in_progress") {
		t.Fatalf("update and add: %+v %+v", r, f.items)
	}
	// the plan and its first in_progress step are one call
	f.items = nil
	r = run(env, `{"add":[{"text":"Read the code","status":"in_progress"},{"text":"Fix it"}]}`)
	if r.IsError || len(f.items) != 2 || f.items[0].Status != event.TodoInProgress || f.items[1].Status != event.TodoPending {
		t.Fatalf("add with a status: %+v %+v", r, f.items)
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

type interruptedAsker struct{}

func (interruptedAsker) Ask(context.Context, []protocol.Question) ([]string, error) {
	return []string{"", "accepted"}, context.Canceled
}

func TestInterruptedQuestionsKeepAcceptedAnswers(t *testing.T) {
	r := Builtin()["ask_user"].Run(context.Background(), json.RawMessage(`{"questions":[{"question":"First?","options":[{"label":"accepted"}]},{"question":"Second?","options":[{"label":"later"}]}]}`), &Env{Ask: interruptedAsker{}})
	if !r.IsError || !strings.Contains(r.Output, "First? → (no answer)") || !strings.Contains(r.Output, "canceled") || !strings.Contains(r.Output, "Second? → accepted") {
		t.Fatalf("partial answers: %+v", r)
	}
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
		if c := clip.Head(s, n); !utf8.ValidString(c) || len(c) > n {
			t.Errorf("clip.Head(%d) = %q", n, c)
		}
		if c := clip.Tail(s, n); !utf8.ValidString(c) || len(c) > n {
			t.Errorf("clip.Tail(%d) = %q", n, c)
		}
	}
	if c := clip.Middle(strings.Repeat("é", 1000), 301); !utf8.ValidString(c) {
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

// until_changed is one background job that runs the command again until its
// output changes, and reports the change once.
func TestShellUntilChanged(t *testing.T) {
	ctx := context.Background()
	mon := &fakeJobs{}
	dir := t.TempDir()
	env := &Env{Dir: dir, Jobs: mon}
	sh := Builtin()["shell"]
	flag := filepath.Join(dir, "flag")
	r := sh.Run(ctx, json.RawMessage(`{"command":"cat flag 2>/dev/null || echo none","until_changed":true}`), env)
	if r.IsError || !strings.Contains(r.Output, "waiting as job m1") || len(mon.adopted) != 1 {
		t.Fatalf("start: %+v adopted=%d", r, len(mon.adopted))
	}
	if mon.specs[0] != "cat flag 2>/dev/null || echo none" {
		t.Fatalf("the job is shown as the command the agent gave, not the loop round it: %q", mon.specs[0])
	}
	job := mon.adopted[0]
	select {
	case <-job.Done():
		t.Fatalf("the job ended with nothing changed: %q", job.Output())
	case <-time.After(300 * time.Millisecond):
	}
	job.Kill() // the loop's first pause is 30 s; the shape is what is checked here
	<-job.Done()
	if r := sh.Run(ctx, json.RawMessage(`{"command":"true","until_changed":true}`), &Env{Dir: dir}); !r.IsError || !strings.Contains(r.Output, "not available") {
		t.Fatalf("without a job runtime: %+v", r)
	}
	_ = flag
}

// The wait loop itself, run directly with a short pause, ends when the
// output changes and says what it was.
func TestUntilChangedLoop(t *testing.T) {
	dir := t.TempDir()
	script := strings.Replace(untilChanged("cat "+filepath.Join(dir, "f")+" 2>/dev/null || echo none", true), `sleep "$__pause"`, `sleep 0.2`, 1)
	go func() {
		time.Sleep(400 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(dir, "f"), []byte("ready"), 0o644)
	}()
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ready") || !strings.Contains(string(out), "[changed: exit status 0, was 0]") {
		t.Fatalf("loop: %v\n%s", err, out)
	}
}

// A truncated output is kept whole where the agent can read it, and the
// result says where.
func TestATruncatedOutputIsKeptWhole(t *testing.T) {
	dir := t.TempDir()
	env := &Env{MaxOutput: 1000, Overflow: filepath.Join(dir, "out")}
	long := strings.Repeat("line of output\n", 200)
	got := env.Clip(long)
	if !strings.Contains(got, "bytes truncated") || !strings.Contains(got, "is at "+filepath.Join(dir, "out")) {
		t.Fatalf("clip: %q", got[len(got)-200:])
	}
	path := got[strings.Index(got, "is at ")+6:]
	path = path[:strings.Index(path, ":")]
	if b, err := os.ReadFile(path); err != nil || string(b) != long {
		t.Fatalf("the whole output was not kept at %s: %v", path, err)
	}
	if short := env.Clip("short"); short != "short" {
		t.Fatalf("a short output was touched: %q", short)
	}
	if got := (&Env{MaxOutput: 1000}).Clip(long); strings.Contains(got, "is at") {
		t.Fatal("with nowhere to keep it, the result names a file")
	}
}
