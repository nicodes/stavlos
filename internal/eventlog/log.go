// Package eventlog stores the append-only event log in SQLite (PRD §4.2).
// The event vocabulary lives in internal/event, which a client can import
// without pulling in a database driver; this package is the daemon's.
//
// One goroutine writes. Appends queue for it and it commits whatever has
// queued in one transaction (group commit), assigning each channel's
// sequence numbers, maintaining the channel index from the events in the
// same transaction, and then handing the committed events to the owner's
// callback in commit order. Readers use a pool of their own, so replaying a
// long channel never holds up an append (WAL lets them run together).
package eventlog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nicodes/stavlos/internal/event"
)

// schemaVersion is PRAGMA user_version for the current schema. A file
// written by any other version is deleted, not converted: stavlos is
// unreleased, so there is nothing to keep, and dropping tables would leave
// a file of free pages behind.
const schemaVersion = 5

// maxGroup bounds how many queued appends one transaction takes.
const maxGroup = 256

// ErrClosed is returned by an append after Close.
var ErrClosed = errors.New("event log is closed")

// Log is the event log.
type Log struct {
	w, r     *sql.DB
	reqs     chan *request
	quit     chan struct{}
	done     chan struct{}
	onCommit func([]event.Event)
	last     map[string]int64 // channel → last seq committed; the writer's alone
}

// request is one append, or a barrier (fn) run between commits.
type request struct {
	evs  []event.Event
	fn   func()
	out  []event.Event
	err  error
	done chan struct{}
}

// Open opens or creates the log at path. onCommit (nil for none) receives
// each committed group's events, in commit order, on the writer goroutine:
// it must not block and must not append.
func Open(path string, onCommit func([]event.Event)) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	w, err := openCurrent(path)
	if err != nil {
		return nil, err
	}
	r, err := sql.Open("sqlite", dsn(path)+"&_pragma=query_only(1)")
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)
	l := &Log{w: w, r: r, reqs: make(chan *request), quit: make(chan struct{}), done: make(chan struct{}), onCommit: onCommit, last: map[string]int64{}}
	go l.run()
	return l, nil
}

func dsn(path string) string {
	return path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
}

// openCurrent opens the writer connection on a database at the current
// schema, deleting a file written by another version first.
func openCurrent(path string) (*sql.DB, error) {
	for attempt := 0; ; attempt++ {
		w, err := sql.Open("sqlite", dsn(path))
		if err != nil {
			return nil, err
		}
		w.SetMaxOpenConns(1)
		version, tables, err := inspect(w)
		if err != nil {
			w.Close()
			return nil, err
		}
		if version == schemaVersion || tables == 0 {
			if err := create(w); err != nil {
				w.Close()
				return nil, err
			}
			return w, nil
		}
		w.Close()
		if attempt > 0 {
			return nil, fmt.Errorf("%s: schema %d could not be replaced", path, version)
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
	}
}

func inspect(w *sql.DB) (version, tables int, err error) {
	if err = w.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return 0, 0, err
	}
	err = w.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table'`).Scan(&tables)
	return version, tables, err
}

func create(w *sql.DB) error {
	_, err := w.Exec(fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS events (
  global  INTEGER PRIMARY KEY,
  channel TEXT    NOT NULL,
  seq     INTEGER NOT NULL,
  agent   TEXT    NOT NULL DEFAULT '',
  type    TEXT    NOT NULL,
  time    INTEGER NOT NULL,
  payload BLOB,
  UNIQUE(channel, seq)
);
CREATE TABLE IF NOT EXISTS channels (
  id       TEXT    PRIMARY KEY,
  name     TEXT    NOT NULL UNIQUE,
  dir      TEXT    NOT NULL,
  created  INTEGER NOT NULL,
  archived INTEGER NOT NULL DEFAULT 0,
  title    TEXT    NOT NULL DEFAULT '',
  last_seq INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS events_type_time ON events(type, time);
CREATE TABLE IF NOT EXISTS trust (
  dir  TEXT PRIMARY KEY,
  hash TEXT NOT NULL
);
PRAGMA user_version = %d;`, schemaVersion))
	return err
}

// Close commits what is queued, stops the writer and closes the database.
func (l *Log) Close() error {
	select {
	case <-l.quit:
	default:
		close(l.quit)
	}
	<-l.done
	err := l.r.Close()
	if werr := l.w.Close(); werr != nil {
		err = werr
	}
	return err
}

// Append writes events in one transaction, all or none, assigning Seq and
// Global, and returns them numbered. Once queued an append is never
// abandoned: ctx is only checked before, so a caller never sees an error for
// an event that was written.
func (l *Log) Append(ctx context.Context, evs ...event.Event) ([]event.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r := &request{evs: evs, done: make(chan struct{})}
	if err := l.submit(r); err != nil {
		return nil, err
	}
	return r.out, r.err
}

// Barrier runs fn on the writer between two commits: every append queued
// before it is committed (and delivered to onCommit) first, and none after
// it starts until fn returns. A subscription hands over from replay to live
// delivery inside one, so it misses nothing and sees nothing twice.
func (l *Log) Barrier(fn func()) error {
	return l.submit(&request{fn: fn, done: make(chan struct{})})
}

func (l *Log) submit(r *request) error {
	select {
	case l.reqs <- r:
	case <-l.quit:
		return ErrClosed
	}
	<-r.done
	return nil
}

// run is the writer: it groups queued requests, commits them in order and
// runs barriers between commits.
func (l *Log) run() {
	defer close(l.done)
	for {
		var group []*request
		select {
		case r := <-l.reqs:
			group = append(group, r)
		case <-l.quit:
			return
		}
	fill:
		for len(group) < maxGroup {
			select {
			case r := <-l.reqs:
				group = append(group, r)
			default:
				break fill
			}
		}
		start := 0
		for i, r := range group {
			if r.fn == nil {
				continue
			}
			l.commit(group[start:i])
			r.fn()
			close(r.done)
			start = i + 1
		}
		l.commit(group[start:])
	}
}

// commit writes a group of appends in one transaction. When the group
// fails, each append is retried alone, so one bad append fails only itself.
func (l *Log) commit(group []*request) {
	if len(group) == 0 {
		return
	}
	if len(group) > 1 {
		if err := l.write(group); err == nil {
			return
		}
	}
	for _, r := range group {
		if err := l.write([]*request{r}); err != nil {
			r.err = err
			close(r.done)
		}
	}
}

// write commits requests in one transaction and, on success, delivers the
// events and releases the callers. On failure nothing is released and the
// sequence cache is left as it was.
func (l *Log) write(group []*request) error {
	ctx := context.Background()
	tx, err := l.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	next := map[string]int64{}
	for _, r := range group {
		r.out = make([]event.Event, 0, len(r.evs))
		for _, e := range r.evs {
			if e.Time.IsZero() {
				e.Time = time.Now().UTC()
			}
			seq, ok := next[e.Channel]
			if !ok {
				if seq, err = l.lastSeq(ctx, tx, e.Channel); err != nil {
					return err
				}
			}
			e.Seq = seq + 1
			res, err := tx.ExecContext(ctx, `INSERT INTO events(channel, seq, agent, type, time, payload) VALUES(?,?,?,?,?,?)`,
				e.Channel, e.Seq, e.Agent, string(e.Type), e.Time.UnixNano(), []byte(e.Payload))
			if err != nil {
				return fmt.Errorf("%s: %w", e.Type, err)
			}
			if e.Global, err = res.LastInsertId(); err != nil {
				return err
			}
			if err := index(ctx, tx, e); err != nil {
				return fmt.Errorf("%s: index: %w", e.Type, err)
			}
			next[e.Channel] = e.Seq
			r.out = append(r.out, e)
		}
	}
	for ch, seq := range next {
		if _, err := tx.ExecContext(ctx, `UPDATE channels SET last_seq = ? WHERE id = ?`, seq, ch); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for ch, seq := range next {
		l.last[ch] = seq
	}
	var all []event.Event
	for _, r := range group {
		all = append(all, r.out...)
	}
	if l.onCommit != nil && len(all) > 0 {
		l.onCommit(all)
	}
	for _, r := range group {
		close(r.done)
	}
	return nil
}

func (l *Log) lastSeq(ctx context.Context, tx *sql.Tx, channel string) (int64, error) {
	if seq, ok := l.last[channel]; ok {
		return seq, nil
	}
	var last sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM events WHERE channel = ?`, channel).Scan(&last)
	return last.Int64, err
}

const eventColumns = `global, channel, seq, agent, type, time, payload`

// Read returns a channel's events with seq >= from, in order; limit <= 0
// reads them all.
func (l *Log) Read(ctx context.Context, channel string, from int64, limit int) ([]event.Event, error) {
	q := `SELECT ` + eventColumns + ` FROM events WHERE channel = ? AND seq >= ? ORDER BY seq`
	args := []any{channel, from}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := l.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []event.Event
	for rows.Next() {
		var e event.Event
		var typ string
		var ns int64
		var payload []byte
		if err := rows.Scan(&e.Global, &e.Channel, &e.Seq, &e.Agent, &typ, &ns, &payload); err != nil {
			return nil, err
		}
		e.Type, e.Time = event.Type(typ), time.Unix(0, ns).UTC()
		if len(payload) > 0 {
			e.Payload = payload
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ChannelRow is a channel's index entry, kept from its events.
type ChannelRow struct {
	ID       string
	Name     string // unique across the daemon
	Dir      string
	Created  time.Time
	Archived bool
	Title    string // the first line of the human's first message
	LastSeq  int64
}

// Channels lists the index, newest first.
func (l *Log) Channels(ctx context.Context) ([]ChannelRow, error) {
	rows, err := l.r.QueryContext(ctx, `SELECT id, name, dir, created, archived, title, last_seq FROM channels ORDER BY created DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChannelRow
	for rows.Next() {
		var c ChannelRow
		var ns int64
		if err := rows.Scan(&c.ID, &c.Name, &c.Dir, &ns, &c.Archived, &c.Title, &c.LastSeq); err != nil {
			return nil, err
		}
		c.Created = time.Unix(0, ns).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

// LastSeq is a channel's latest committed sequence number (0 for none).
func (l *Log) LastSeq(ctx context.Context, channel string) (int64, error) {
	var seq int64
	err := l.r.QueryRowContext(ctx, `SELECT last_seq FROM channels WHERE id = ?`, channel).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return seq, err
}

// Trust lists the confirmed project layers: directory → content hash.
func (l *Log) Trust(ctx context.Context) (map[string]string, error) {
	rows, err := l.r.QueryContext(ctx, `SELECT dir, hash FROM trust`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var dir, hash string
		if err := rows.Scan(&dir, &hash); err != nil {
			return nil, err
		}
		out[dir] = hash
	}
	return out, rows.Err()
}

// SetTrust records that dir's project layer with this hash is trusted.
func (l *Log) SetTrust(ctx context.Context, dir, hash string) error {
	_, err := l.w.ExecContext(ctx, `INSERT INTO trust(dir, hash) VALUES(?, ?) ON CONFLICT(dir) DO UPDATE SET hash = excluded.hash`, dir, hash)
	return err
}
