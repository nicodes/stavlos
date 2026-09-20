package eventlog

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nicodes/stavlos/internal/event"
)

func open(t *testing.T, path string, onCommit func([]event.Event)) *Log {
	t.Helper()
	l, err := Open(path, onCommit)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func created(channel, name string) event.Event {
	return event.Event{Channel: channel, Type: event.ChannelCreated, Payload: event.MustPayload(event.ChannelCreatedPayload{Name: name, Dir: "/w"})}
}

// prompt is an input for an agent of channel: the human's when source is
// "human:…", another agent's request otherwise.
func prompt(channel, source, text string) event.Event {
	in := event.Input{ID: text, Kind: event.InputPrompt, Text: text}
	if !strings.HasPrefix(source, "human:") {
		in.Kind, in.From = event.InputRequest, source
	}
	return event.Event{Channel: channel, Agent: "a1", Type: event.InputQueued, Payload: event.MustPayload(in)}
}

func appendOne(t *testing.T, l *Log, e event.Event) event.Event {
	t.Helper()
	out, err := l.Append(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	return out[0]
}

func TestAppendSeqContiguousPerChannel(t *testing.T) {
	l := open(t, filepath.Join(t.TempDir(), "e.db"), nil)
	defer l.Close()
	ctx := context.Background()
	appendOne(t, l, created("a", "a"))
	appendOne(t, l, created("b", "b"))
	for i := 0; i < 3; i++ {
		appendOne(t, l, prompt("a", "agent:x", "x"))
		appendOne(t, l, prompt("b", "agent:x", "y"))
	}
	evs, err := l.Read(ctx, "a", 1, 0)
	if err != nil || len(evs) != 4 {
		t.Fatalf("read %d %v", len(evs), err)
	}
	for i, e := range evs {
		if e.Seq != int64(i+1) || e.Time.IsZero() {
			t.Fatalf("event %d: %+v", i, e)
		}
	}
	var tp event.Input
	if err := evs[1].Decode(&tp); err != nil || tp.Text != "x" {
		t.Fatal("payload round trip")
	}
	if seq, _ := l.LastSeq(ctx, "b"); seq != 4 {
		t.Fatalf("last seq %d", seq)
	}
	if evs, _ := l.Read(ctx, "a", 2, 1); len(evs) != 1 || evs[0].Seq != 2 {
		t.Fatalf("from and limit: %+v", evs)
	}
}

// TestGroupCommitDeliversInOrder: concurrent appends are grouped, every
// channel's numbers stay contiguous, a batch is contiguous, and onCommit
// sees every event once in commit (global) order.
func TestGroupCommitDeliversInOrder(t *testing.T) {
	var mu sync.Mutex
	var seen []event.Event
	l := open(t, filepath.Join(t.TempDir(), "e.db"), func(evs []event.Event) {
		mu.Lock()
		seen = append(seen, evs...)
		mu.Unlock()
	})
	defer l.Close()
	ctx := context.Background()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			ch := fmt.Sprintf("c%d", w%3)
			for i := 0; i < 25; i++ {
				out, err := l.Append(ctx, prompt(ch, "agent:x", "a"), prompt(ch, "agent:x", "b"))
				if err != nil || len(out) != 2 || out[1].Seq != out[0].Seq+1 {
					t.Errorf("batch: %+v %v", out, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 400 {
		t.Fatalf("delivered %d events", len(seen))
	}
	next := map[string]int64{}
	for i, e := range seen {
		if i > 0 && e.Global <= seen[i-1].Global {
			t.Fatalf("out of order at %d: %d after %d", i, e.Global, seen[i-1].Global)
		}
		if e.Seq != next[e.Channel]+1 {
			t.Fatalf("%s: seq %d after %d", e.Channel, e.Seq, next[e.Channel])
		}
		next[e.Channel] = e.Seq
	}
}

// TestBarrier: a barrier runs after everything queued before it is
// committed and delivered, and an append made meanwhile waits for it.
func TestBarrier(t *testing.T) {
	delivered := make(chan int64, 16)
	l := open(t, filepath.Join(t.TempDir(), "e.db"), func(evs []event.Event) {
		for _, e := range evs {
			delivered <- e.Seq
		}
	})
	defer l.Close()
	ctx := context.Background()
	appendOne(t, l, prompt("c", "agent:x", "1"))
	<-delivered
	release := make(chan struct{})
	inside := make(chan struct{})
	go func() {
		_ = l.Barrier(func() {
			close(inside)
			<-release
		})
	}()
	<-inside
	appended := make(chan error, 1)
	go func() {
		_, err := l.Append(ctx, prompt("c", "agent:x", "2"))
		appended <- err
	}()
	select {
	case <-appended:
		t.Fatal("an append committed while a barrier ran")
	case <-delivered:
		t.Fatal("an event was delivered while a barrier ran")
	default:
	}
	close(release)
	if err := <-appended; err != nil {
		t.Fatal(err)
	}
	if seq := <-delivered; seq != 2 {
		t.Fatalf("seq %d", seq)
	}
}

// TestChannelIndex: the channels table follows the events in their own
// transaction: channel.created inserts the row (a taken name fails that
// append alone), a rename and an archive update it, the title is the first
// line of the human's first message, and last_seq follows every append.
func TestChannelIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e.db")
	l := open(t, path, nil)
	ctx := context.Background()
	appendOne(t, l, created("s1", "docs"))
	for _, e := range []event.Event{
		prompt("s1", "agent:a1", "an agent's task is not a title"),
		prompt("s1", "human:tui", "  \n"),
		prompt("s1", "human:tui", "fix the login bug\nand add tests"),
		prompt("s1", "human:tui", "a later prompt"),
		{Channel: "s1", Type: event.ChannelUpdated, Payload: event.MustPayload(event.ChannelUpdatedPayload{Name: event.Str("web")})},
		{Channel: "s1", Type: event.ChannelArchived},
	} {
		appendOne(t, l, e)
	}
	if _, err := l.Append(ctx, created("s2", "web")); err == nil {
		t.Fatal("a second channel took a taken name")
	}
	if _, err := l.Append(ctx, created("s3", "api")); err != nil {
		t.Fatalf("a failed append took another with it: %v", err)
	}
	l.Close()
	l = open(t, path, nil) // the index survives a reopen
	defer l.Close()
	rows, err := l.Channels(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("channels %+v %v", rows, err)
	}
	byID := map[string]ChannelRow{rows[0].ID: rows[0], rows[1].ID: rows[1]}
	if r := byID["s1"]; r.Name != "web" || r.Title != "fix the login bug" || r.LastSeq != 7 || !r.Archived || r.Dir != "/w" || r.Created.IsZero() {
		t.Fatalf("row %+v", r)
	}
	if e := appendOne(t, l, prompt("s1", "agent:x", "after reopen")); e.Seq != 8 {
		t.Fatalf("numbering after reopen: %d", e.Seq)
	}
}

func TestTrust(t *testing.T) {
	l := open(t, filepath.Join(t.TempDir(), "e.db"), nil)
	defer l.Close()
	ctx := context.Background()
	if err := l.SetTrust(ctx, "/w", "h1"); err != nil {
		t.Fatal(err)
	}
	if err := l.SetTrust(ctx, "/w", "h2"); err != nil {
		t.Fatal(err)
	}
	if m, err := l.Trust(ctx); err != nil || len(m) != 1 || m["/w"] != "h2" {
		t.Fatalf("trust %v %v", m, err)
	}
}

// oldLog writes a database at schema version with a table nobody reads now.
func oldLog(t *testing.T, version int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf(`
CREATE TABLE sessions (id TEXT PRIMARY KEY, dir TEXT NOT NULL);
INSERT INTO sessions VALUES ('s1', '/w');
PRAGMA user_version = %d;`, version)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	return path
}

// TestALogItCannotReadIsLeftAlone: a log is history somebody has. One
// written by a schema with no way forward (older than the first migration,
// or newer than this build) is reported and left exactly as it is; only an
// explicit reset deletes it.
func TestALogItCannotReadIsLeftAlone(t *testing.T) {
	for _, version := range []int{baseVersion - 1, schemaVersion + 1} {
		path := oldLog(t, version)
		before, _ := os.ReadFile(path)
		if _, err := Open(path, nil); err == nil || !strings.Contains(err.Error(), ResetEnv) {
			t.Fatalf("schema %d: opened, or the error does not say what to do: %v", version, err)
		}
		if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
			t.Fatalf("schema %d: the file was changed", version)
		}
	}
	path := oldLog(t, baseVersion-1)
	t.Setenv(ResetEnv, "1")
	l := open(t, path, nil)
	defer l.Close()
	var n int
	if err := l.r.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM sqlite_master WHERE name = 'sessions'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("reset left the old table: %d %v", n, err)
	}
	if e := appendOne(t, l, created("c1", "c1")); e.Seq != 1 || e.Global != 1 {
		t.Fatalf("append after a reset %+v", e)
	}
}

// TestAnOlderLogIsConvertedAndKept: a schema change is a migration. The
// events survive it, a copy of the file as it was is kept beside it, each
// step runs once, and a step that fails leaves the log at the version it had.
func TestAnOlderLogIsConvertedAndKept(t *testing.T) {
	defer func(m []migration, v int) { migrations, schemaVersion = m, v }(migrations, schemaVersion)
	path := filepath.Join(t.TempDir(), "e.db")
	l := open(t, path, nil)
	appendOne(t, l, created("c1", "c1"))
	appendOne(t, l, prompt("c1", "human:x", "kept"))
	l.Close()

	ran := 0
	migrations = []migration{
		func(tx *sql.Tx) error {
			ran++
			_, err := tx.Exec(`ALTER TABLE channels ADD COLUMN note TEXT NOT NULL DEFAULT ''`)
			return err
		},
		func(tx *sql.Tx) error { ran++; _, err := tx.Exec(`UPDATE channels SET note = 'migrated'`); return err },
	}
	schemaVersion = baseVersion + len(migrations)
	l = open(t, path, nil)
	ctx := context.Background()
	evs, err := l.Read(ctx, "c1", 1, 0)
	var note string
	if err != nil || len(evs) != 2 || l.r.QueryRowContext(ctx, `SELECT note FROM channels WHERE id = 'c1'`).Scan(&note) != nil || note != "migrated" || ran != 2 {
		t.Fatalf("after converting: %d events, note %q, %d steps, %v", len(evs), note, ran, err)
	}
	if e := appendOne(t, l, prompt("c1", "human:x", "after")); e.Seq != 3 {
		t.Fatalf("append after converting: %+v", e)
	}
	l.Close()
	if _, err := os.Stat(fmt.Sprintf("%s.v%d.bak", path, baseVersion)); err != nil {
		t.Fatalf("no copy of the log as it was: %v", err)
	}
	l = open(t, path, nil) // already current: nothing runs again
	l.Close()
	if ran != 2 {
		t.Fatalf("a migration ran twice: %d", ran)
	}

	migrations = append(migrations, func(tx *sql.Tx) error { _, _ = tx.Exec(`DELETE FROM events`); return errors.New("boom") })
	schemaVersion = baseVersion + len(migrations)
	if _, err := Open(path, nil); err == nil || !strings.Contains(err.Error(), "unchanged") {
		t.Fatalf("a failing step: %v", err)
	}
	migrations, schemaVersion = migrations[:2], baseVersion+2
	l = open(t, path, nil)
	defer l.Close()
	if evs, err := l.Read(ctx, "c1", 1, 0); err != nil || len(evs) != 3 {
		t.Fatalf("a failed step cost events: %d %v", len(evs), err)
	}
}

func TestAppendAfterClose(t *testing.T) {
	l := open(t, filepath.Join(t.TempDir(), "e.db"), nil)
	appendOne(t, l, prompt("c", "agent:x", "1"))
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(context.Background(), prompt("c", "agent:x", "2")); !errors.Is(err, ErrClosed) {
		t.Fatalf("append after close: %v", err)
	}
}
