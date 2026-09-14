package event

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Log is the append-only SQLite event log (PRD §4.2). It is safe for
// concurrent use. Appends are serialised so per-session sequences stay
// contiguous.
type Log struct {
	db *sql.DB
	mu sync.Mutex // serialises Append
}

// Open opens or creates the log at path.
func Open(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	l := &Log{db: db}
	if err := l.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return l, nil
}

func (l *Log) migrate() error {
	_, err := l.db.Exec(`
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
CREATE INDEX IF NOT EXISTS events_session ON events(session, seq);
CREATE TABLE IF NOT EXISTS sessions (
  id       TEXT PRIMARY KEY,
  dir      TEXT NOT NULL,
  created  TEXT NOT NULL,
  archived INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS kv (
  k TEXT PRIMARY KEY,
  v BLOB
);`)
	return err
}

// Close closes the database.
func (l *Log) Close() error { return l.db.Close() }

// Append writes one event, assigning Seq and Global. The returned event
// carries the assigned numbers; delivering it to clients is the daemon's.
func (l *Log) Append(ctx context.Context, e Event) (Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return e, err
	}
	defer tx.Rollback()
	var last sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM events WHERE session=?`, e.Session).Scan(&last); err != nil {
		return e, err
	}
	e.Seq = last.Int64 + 1
	res, err := tx.ExecContext(ctx, `INSERT INTO events(session, seq, agent, type, time, payload) VALUES(?,?,?,?,?,?)`,
		e.Session, e.Seq, e.Agent, string(e.Type), e.Time.Format(time.RFC3339Nano), []byte(e.Payload))
	if err != nil {
		return e, err
	}
	e.Global, _ = res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return e, err
	}
	return e, nil
}

// Read returns events for a session with seq >= from, in order. limit<=0 = all.
func (l *Log) Read(ctx context.Context, session string, from int64, limit int) ([]Event, error) {
	q := `SELECT global, session, seq, agent, type, time, payload FROM events WHERE session=? AND seq>=? ORDER BY seq`
	args := []any{session, from}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := l.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ReadRange returns events with from <= seq <= to.
func (l *Log) ReadRange(ctx context.Context, session string, from, to int64) ([]Event, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT global, session, seq, agent, type, time, payload FROM events WHERE session=? AND seq>=? AND seq<=? ORDER BY seq`, session, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LastSeq returns the latest per-session sequence (0 if none).
func (l *Log) LastSeq(ctx context.Context, session string) (int64, error) {
	var last sql.NullInt64
	err := l.db.QueryRowContext(ctx, `SELECT MAX(seq) FROM events WHERE session=?`, session).Scan(&last)
	return last.Int64, err
}

type scanner interface {
	Scan(dest ...any) error
}

func scan(r scanner) (Event, error) {
	var e Event
	var typ, ts string
	var payload []byte
	if err := r.Scan(&e.Global, &e.Session, &e.Seq, &e.Agent, &typ, &ts, &payload); err != nil {
		return e, err
	}
	e.Type = Type(typ)
	e.Time, _ = time.Parse(time.RFC3339Nano, ts)
	if len(payload) > 0 {
		e.Payload = json.RawMessage(payload)
	}
	return e, nil
}

// --- session index (not session state; PRD §4.2) ---

// SessionRow is the daemon-level index entry for a session.
type SessionRow struct {
	ID       string
	Dir      string
	Created  time.Time
	Archived bool
}

// PutSession inserts or updates a session index row.
func (l *Log) PutSession(ctx context.Context, s SessionRow) error {
	_, err := l.db.ExecContext(ctx, `INSERT INTO sessions(id, dir, created, archived) VALUES(?,?,?,?)
ON CONFLICT(id) DO UPDATE SET dir=excluded.dir, archived=excluded.archived`,
		s.ID, s.Dir, s.Created.UTC().Format(time.RFC3339Nano), boolInt(s.Archived))
	return err
}

// Sessions lists index rows, newest first.
func (l *Log) Sessions(ctx context.Context) ([]SessionRow, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT id, dir, created, archived FROM sessions ORDER BY created DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var s SessionRow
		var ts string
		var arch int
		if err := rows.Scan(&s.ID, &s.Dir, &ts, &arch); err != nil {
			return nil, err
		}
		s.Created, _ = time.Parse(time.RFC3339Nano, ts)
		s.Archived = arch != 0
		out = append(out, s)
	}
	return out, rows.Err()
}

// Session fetches one index row.
func (l *Log) Session(ctx context.Context, id string) (SessionRow, error) {
	var s SessionRow
	var ts string
	var arch int
	err := l.db.QueryRowContext(ctx, `SELECT id, dir, created, archived FROM sessions WHERE id=?`, id).Scan(&s.ID, &s.Dir, &ts, &arch)
	if errors.Is(err, sql.ErrNoRows) {
		return s, fmt.Errorf("session %q not found", id)
	}
	if err != nil {
		return s, err
	}
	s.Created, _ = time.Parse(time.RFC3339Nano, ts)
	s.Archived = arch != 0
	return s, nil
}

// --- kv (trust records etc.) ---

// Get reads a kv value; ok=false if absent.
func (l *Log) Get(ctx context.Context, key string, v any) (bool, error) {
	var b []byte
	err := l.db.QueryRowContext(ctx, `SELECT v FROM kv WHERE k=?`, key).Scan(&b)
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
	_, err = l.db.ExecContext(ctx, `INSERT INTO kv(k, v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, key, b)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
