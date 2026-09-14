// Package eventlog stores the append-only event log in SQLite (PRD §4.2).
// The event vocabulary lives in internal/event, which a client can import
// without pulling in a database driver; this package is the daemon's.
package eventlog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nicodes/stavlos/internal/event"
)

// schemaVersion is PRAGMA user_version once migrate has run.
const schemaVersion = 2

// Log is the event log. Appends go through one writer connection and are
// serialised so per-session sequences stay contiguous; reads use a small
// pool of their own, so replaying a long session to a client never holds
// up an agent's append (WAL lets readers and the writer run together).
type Log struct {
	w *sql.DB
	r *sql.DB

	mu   sync.Mutex       // serialises appends; guards last
	last map[string]int64 // session → last seq written, filled lazily
}

// Open opens or creates the log at path and brings its schema up to date.
func Open(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	const pragmas = "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	w, err := sql.Open("sqlite", path+pragmas)
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	l := &Log{w: w, last: map[string]int64{}}
	if err := l.migrate(context.Background()); err != nil {
		w.Close()
		return nil, err
	}
	r, err := sql.Open("sqlite", path+pragmas+"&_pragma=query_only(1)")
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)
	l.r = r
	return l, nil
}

func (l *Log) migrate(ctx context.Context) error {
	if _, err := l.w.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS events (
  global  INTEGER PRIMARY KEY AUTOINCREMENT,
  session TEXT NOT NULL,
  seq     INTEGER NOT NULL,
  agent   TEXT NOT NULL DEFAULT '',
  type    TEXT NOT NULL,
  time    TEXT NOT NULL,
  payload BLOB,
  UNIQUE(session, seq)
);
CREATE TABLE IF NOT EXISTS sessions (
  id       TEXT PRIMARY KEY,
  dir      TEXT NOT NULL,
  created  TEXT NOT NULL,
  archived INTEGER NOT NULL DEFAULT 0,
  title    TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS kv (
  k TEXT PRIMARY KEY,
  v BLOB
);`); err != nil {
		return err
	}
	var version int
	if err := l.w.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version >= schemaVersion {
		return nil
	}
	// A log from before version 2: the session index gains its title (so
	// listing sessions is one query), backfilled once from the events, and
	// the index that duplicated UNIQUE(session, seq) goes.
	if _, err := l.w.ExecContext(ctx, `SELECT title FROM sessions LIMIT 0`); err != nil {
		if _, err := l.w.ExecContext(ctx, `ALTER TABLE sessions ADD COLUMN title TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate sessions: %w", err)
		}
	}
	if err := l.backfillTitles(ctx); err != nil {
		return err
	}
	if _, err := l.w.ExecContext(ctx, `DROP INDEX IF EXISTS events_session`); err != nil {
		return err
	}
	_, err := l.w.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion))
	return err
}

func (l *Log) backfillTitles(ctx context.Context) error {
	rows, err := l.w.QueryContext(ctx, `SELECT id FROM sessions WHERE title = ''`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		prompts, err := l.w.QueryContext(ctx, `SELECT type, payload FROM events WHERE session = ? AND type IN (?, ?) ORDER BY seq`, id, string(event.PromptQueued), string(event.SteerReceived))
		if err != nil {
			return err
		}
		title := ""
		for title == "" && prompts.Next() {
			var typ string
			var payload []byte
			if err := prompts.Scan(&typ, &payload); err != nil {
				prompts.Close()
				return err
			}
			title = titleOf(event.Event{Type: event.Type(typ), Payload: payload})
		}
		prompts.Close()
		if title != "" {
			if _, err := l.w.ExecContext(ctx, `UPDATE sessions SET title = ? WHERE id = ?`, title, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// Close closes the database.
func (l *Log) Close() error {
	var err error
	if l.r != nil {
		err = l.r.Close()
	}
	if werr := l.w.Close(); werr != nil {
		err = werr
	}
	return err
}

// Append writes one event, assigning Seq and Global. The returned event
// carries the assigned numbers; delivering it to clients is the daemon's.
func (l *Log) Append(ctx context.Context, e event.Event) (event.Event, error) {
	out, err := l.AppendBatch(ctx, []event.Event{e}, nil)
	if err != nil {
		return e, err
	}
	return out[0], nil
}

// AppendBatch writes events in one transaction: all of them or none. When
// newSession is set its index row is inserted in the same transaction
// first, so a copied session (a fork) appears whole or not at all.
func (l *Log) AppendBatch(ctx context.Context, evs []event.Event, newSession *SessionRow) ([]event.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if newSession != nil {
		if err := putSession(ctx, tx, *newSession); err != nil {
			return nil, err
		}
	}
	next := map[string]int64{} // this batch's view of the last seq per session
	out := make([]event.Event, 0, len(evs))
	for _, e := range evs {
		if e.Time.IsZero() {
			e.Time = time.Now().UTC()
		}
		last, ok := next[e.Session]
		if !ok {
			if last, err = l.lastSeqTx(ctx, tx, e.Session); err != nil {
				return nil, err
			}
		}
		e.Seq = last + 1
		res, err := tx.ExecContext(ctx, `INSERT INTO events(session, seq, agent, type, time, payload) VALUES(?,?,?,?,?,?)`,
			e.Session, e.Seq, e.Agent, string(e.Type), e.Time.Format(time.RFC3339Nano), []byte(e.Payload))
		if err != nil {
			return nil, err
		}
		e.Global, _ = res.LastInsertId()
		next[e.Session] = e.Seq
		if title := titleOf(e); title != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET title = ? WHERE id = ? AND title = ''`, title, e.Session); err != nil {
				return nil, err
			}
		}
		out = append(out, e)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for s, seq := range next {
		l.last[s] = seq
	}
	return out, nil
}

// lastSeqTx is the session's last seq: the cache, else the table. The
// caller holds l.mu.
func (l *Log) lastSeqTx(ctx context.Context, tx *sql.Tx, session string) (int64, error) {
	if seq, ok := l.last[session]; ok {
		return seq, nil
	}
	var last sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM events WHERE session = ?`, session).Scan(&last); err != nil {
		return 0, err
	}
	return last.Int64, nil
}

// titleOf is a session's title if e can give it one: the first line of a
// human's prompt or steer.
func titleOf(e event.Event) string {
	if e.Type != event.PromptQueued && e.Type != event.SteerReceived {
		return ""
	}
	var p event.TextPayload
	if e.Decode(&p) != nil || !strings.HasPrefix(p.Source, "human:") {
		return ""
	}
	t := strings.TrimSpace(p.Text)
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	return t
}

const eventColumns = `global, session, seq, agent, type, time, payload`

// Read returns events for a session with seq >= from, in order. limit<=0 = all.
func (l *Log) Read(ctx context.Context, session string, from int64, limit int) ([]event.Event, error) {
	q := `SELECT ` + eventColumns + ` FROM events WHERE session = ? AND seq >= ? ORDER BY seq`
	args := []any{session, from}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	return l.query(ctx, q, args...)
}

// ReadRange returns events with from <= seq <= to.
func (l *Log) ReadRange(ctx context.Context, session string, from, to int64) ([]event.Event, error) {
	return l.query(ctx, `SELECT `+eventColumns+` FROM events WHERE session = ? AND seq >= ? AND seq <= ? ORDER BY seq`, session, from, to)
}

func (l *Log) query(ctx context.Context, q string, args ...any) ([]event.Event, error) {
	rows, err := l.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []event.Event
	for rows.Next() {
		var e event.Event
		var typ, ts string
		var payload []byte
		if err := rows.Scan(&e.Global, &e.Session, &e.Seq, &e.Agent, &typ, &ts, &payload); err != nil {
			return nil, err
		}
		e.Type = event.Type(typ)
		e.Time, _ = time.Parse(time.RFC3339Nano, ts)
		if len(payload) > 0 {
			e.Payload = json.RawMessage(payload)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LastSeq returns the latest per-session sequence (0 if none).
func (l *Log) LastSeq(ctx context.Context, session string) (int64, error) {
	l.mu.Lock()
	seq, ok := l.last[session]
	l.mu.Unlock()
	if ok {
		return seq, nil
	}
	var last sql.NullInt64
	err := l.r.QueryRowContext(ctx, `SELECT MAX(seq) FROM events WHERE session = ?`, session).Scan(&last)
	return last.Int64, err
}

// --- session index (not session state; PRD §4.2) ---

// SessionRow is the daemon-level index entry for a session. Title and
// LastSeq are read-only here: the log keeps them from the events.
type SessionRow struct {
	ID       string
	Dir      string
	Created  time.Time
	Archived bool
	Title    string // the first human prompt's first line
	LastSeq  int64
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func putSession(ctx context.Context, x execer, s SessionRow) error {
	_, err := x.ExecContext(ctx, `INSERT INTO sessions(id, dir, created, archived, title) VALUES(?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET dir = excluded.dir, archived = excluded.archived`,
		s.ID, s.Dir, s.Created.UTC().Format(time.RFC3339Nano), boolInt(s.Archived), s.Title)
	return err
}

// PutSession inserts a session index row, or updates its directory and
// archived flag (the title is never overwritten).
func (l *Log) PutSession(ctx context.Context, s SessionRow) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return putSession(ctx, l.w, s)
}

// Sessions lists index rows, newest first, with their titles and last seq
// in the same query.
func (l *Log) Sessions(ctx context.Context) ([]SessionRow, error) {
	rows, err := l.r.QueryContext(ctx, `SELECT s.id, s.dir, s.created, s.archived, s.title,
  COALESCE((SELECT MAX(e.seq) FROM events e WHERE e.session = s.id), 0)
FROM sessions s ORDER BY s.created DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var s SessionRow
		var ts string
		var arch int
		if err := rows.Scan(&s.ID, &s.Dir, &ts, &arch, &s.Title, &s.LastSeq); err != nil {
			return nil, err
		}
		s.Created, _ = time.Parse(time.RFC3339Nano, ts)
		s.Archived = arch != 0
		out = append(out, s)
	}
	return out, rows.Err()
}

// --- kv (trust records etc.) ---

// Get reads a kv value; ok=false if absent.
func (l *Log) Get(ctx context.Context, key string, v any) (bool, error) {
	var b []byte
	err := l.r.QueryRowContext(ctx, `SELECT v FROM kv WHERE k = ?`, key).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

// Put writes a kv value.
func (l *Log) Put(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err = l.w.ExecContext(ctx, `INSERT INTO kv(k, v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, key, b)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
