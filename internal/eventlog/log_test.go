package eventlog

import (
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

// TestReplacesAnOldLog: a file written by another schema version is
// deleted and created afresh, not converted or left full of free pages.
func TestReplacesAnOldLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
CREATE TABLE events (global INTEGER PRIMARY KEY AUTOINCREMENT, session TEXT NOT NULL, seq INTEGER NOT NULL, agent TEXT NOT NULL DEFAULT '', type TEXT NOT NULL, time TEXT NOT NULL, payload BLOB, UNIQUE(session, seq));
CREATE TABLE sessions (id TEXT PRIMARY KEY, dir TEXT NOT NULL, created TEXT NOT NULL, archived INTEGER NOT NULL DEFAULT 0, title TEXT NOT NULL DEFAULT '');
INSERT INTO sessions VALUES ('s1', '/w', '2026-09-01T00:00:00Z', 0, 'old');
PRAGMA user_version = 4;
`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	l := open(t, path, nil)
	defer l.Close()
	ctx := context.Background()
	var n int
	if err := l.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sessions'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the old table is still there: %d %v", n, err)
	}
	var version int
	if err := l.w.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("user_version %d %v", version, err)
	}
	if e := appendOne(t, l, created("c1", "c1")); e.Seq != 1 || e.Global != 1 {
		t.Fatalf("append after the replacement %+v", e)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
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
