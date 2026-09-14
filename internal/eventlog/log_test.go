package eventlog

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/event"
)

func open(t *testing.T, path string) *Log {
	t.Helper()
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func prompt(session, source, text string) event.Event {
	return event.Event{Session: session, Type: event.PromptQueued, Payload: event.MustPayload(event.TextPayload{Text: text, Source: source})}
}

func TestAppendSeqContiguousPerSession(t *testing.T) {
	l := open(t, filepath.Join(t.TempDir(), "e.db"))
	defer l.Close()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := l.Append(ctx, event.Event{Session: "a", Type: event.PromptQueued, Payload: event.MustPayload(event.TextPayload{Text: "x"})}); err != nil {
			t.Fatal(err)
		}
		if _, err := l.Append(ctx, event.Event{Session: "b", Type: event.PromptQueued}); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := l.Read(ctx, "a", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Fatalf("want 3 got %d", len(evs))
	}
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("seq %d != %d", e.Seq, i+1)
		}
		if e.Global != int64(2*i+1) {
			t.Fatalf("global %d != %d", e.Global, 2*i+1)
		}
	}
	var tp event.TextPayload
	if err := evs[0].Decode(&tp); err != nil || tp.Text != "x" {
		t.Fatal("payload roundtrip")
	}
	if seq, _ := l.LastSeq(ctx, "b"); seq != 3 {
		t.Fatalf("last seq %d", seq)
	}
	if r, _ := l.ReadRange(ctx, "a", 2, 2); len(r) != 1 || r[0].Seq != 2 {
		t.Fatalf("range %+v", r)
	}
}

// TestLastSeqAfterReopen: the cache starts empty on open, so the next
// append continues from what is on disk.
func TestLastSeqAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e.db")
	ctx := context.Background()
	l := open(t, path)
	for i := 0; i < 3; i++ {
		if _, err := l.Append(ctx, event.Event{Session: "a", Type: event.PromptQueued}); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()
	l = open(t, path)
	defer l.Close()
	if seq, _ := l.LastSeq(ctx, "a"); seq != 3 {
		t.Fatalf("last seq after reopen %d", seq)
	}
	e, err := l.Append(ctx, event.Event{Session: "a", Type: event.PromptQueued})
	if err != nil || e.Seq != 4 {
		t.Fatalf("append after reopen: %+v %v", e, err)
	}
}

// TestSessionIndex: the title is the first human prompt's first line, set
// once; the listing carries it and the last seq; a batch with a new
// session row is one transaction.
func TestSessionIndex(t *testing.T) {
	l := open(t, filepath.Join(t.TempDir(), "e.db"))
	defer l.Close()
	ctx := context.Background()
	created := time.Now().UTC()
	if err := l.PutSession(ctx, SessionRow{ID: "s1", Dir: "/w", Created: created}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []event.Event{
		prompt("s1", "agent:a1", "an agent's task is not a title"),
		prompt("s1", "human:tui", "  \n"),
		prompt("s1", "human:tui", "fix the login bug\nand add tests"),
		prompt("s1", "human:tui", "a later prompt"),
	} {
		if _, err := l.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	// Archiving keeps the title.
	if err := l.PutSession(ctx, SessionRow{ID: "s1", Dir: "/w", Created: created, Archived: true}); err != nil {
		t.Fatal(err)
	}
	// A fork: the row and its copied events in one transaction.
	copied, err := l.AppendBatch(ctx, []event.Event{
		prompt("s2", "human:tui", "forked title"),
		{Session: "s2", Type: event.TurnStarted},
	}, &SessionRow{ID: "s2", Dir: "/w", Created: created.Add(time.Second)})
	if err != nil || len(copied) != 2 || copied[0].Seq != 1 || copied[1].Seq != 2 {
		t.Fatalf("batch: %+v %v", copied, err)
	}
	rows, err := l.Sessions(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("sessions %+v %v", rows, err)
	}
	if r := rows[0]; r.ID != "s2" || r.Title != "forked title" || r.LastSeq != 2 || r.Archived {
		t.Fatalf("newest %+v", r)
	}
	if r := rows[1]; r.ID != "s1" || r.Title != "fix the login bug" || r.LastSeq != 4 || !r.Archived || r.Dir != "/w" {
		t.Fatalf("older %+v", r)
	}
	// kv round trip
	if err := l.Put(ctx, "trust", map[string]string{"/w": "h"}); err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if ok, err := l.Get(ctx, "trust", &m); !ok || err != nil || m["/w"] != "h" {
		t.Fatalf("kv %v %v %v", ok, err, m)
	}
	if ok, _ := l.Get(ctx, "missing", &m); ok {
		t.Fatal("missing key found")
	}
}

// TestMigratesAnOldLog: a log written before schema version 2 gains the
// title column (backfilled from its events) and loses the duplicate index.
func TestMigratesAnOldLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
CREATE TABLE events (global INTEGER PRIMARY KEY AUTOINCREMENT, session TEXT NOT NULL, seq INTEGER NOT NULL, agent TEXT NOT NULL DEFAULT '', type TEXT NOT NULL, time TEXT NOT NULL, payload BLOB, UNIQUE(session, seq));
CREATE INDEX events_session ON events(session, seq);
CREATE TABLE sessions (id TEXT PRIMARY KEY, dir TEXT NOT NULL, created TEXT NOT NULL, archived INTEGER NOT NULL DEFAULT 0);
CREATE TABLE kv (k TEXT PRIMARY KEY, v BLOB);
INSERT INTO sessions VALUES ('s1', '/w', '2026-09-01T00:00:00Z', 0);
INSERT INTO events(session, seq, type, time, payload) VALUES ('s1', 1, 'session.created', '2026-09-01T00:00:00Z', '{}');
INSERT INTO events(session, seq, type, time, payload) VALUES ('s1', 2, 'prompt.queued', '2026-09-01T00:00:01Z', '{"text":"old session title\nmore","source":"human:tui"}');
`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	l := open(t, path)
	defer l.Close()
	ctx := context.Background()
	rows, err := l.Sessions(ctx)
	if err != nil || len(rows) != 1 || rows[0].Title != "old session title" || rows[0].LastSeq != 2 {
		t.Fatalf("migrated sessions %+v %v", rows, err)
	}
	var n int
	if err := l.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'events_session'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("duplicate index still there: %d %v", n, err)
	}
	var version int
	if err := l.w.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("user_version %d %v", version, err)
	}
	if e, err := l.Append(ctx, event.Event{Session: "s1", Type: event.TurnStarted}); err != nil || e.Seq != 3 {
		t.Fatalf("append after migration %+v %v", e, err)
	}
}
