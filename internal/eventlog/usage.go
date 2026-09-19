package eventlog

import (
	"context"
	"database/sql"
	"github.com/nicodes/stavlos/internal/event"
	"strings"
	"time"
)

// The usage table is a projection of the model calls in the log, one narrow
// row a call, written in the transaction that appends the call (index.go).
// Charts and the cache share read it and never a payload. They used to
// parse JSON out of every assistant.message in range, which is where a
// model's whole reply lives: 41 MB of a four-day log, read again every three
// seconds while a chart was open, with a channel filter that was no index at
// all.
const usageSchema = `
CREATE TABLE IF NOT EXISTS usage (
  channel  TEXT    NOT NULL,
  agent    TEXT    NOT NULL DEFAULT '',
  provider TEXT    NOT NULL DEFAULT '',
  time     INTEGER NOT NULL,
  fresh    INTEGER NOT NULL DEFAULT 0, -- input tokens charged as new
  cached   INTEGER NOT NULL DEFAULT 0, -- input tokens served from the provider's cache
  output   INTEGER NOT NULL DEFAULT 0,
  cost     REAL    NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS usage_time ON usage(time);
CREATE INDEX IF NOT EXISTS usage_channel_time ON usage(channel, time);
`

// usageBackfill fills the table from a log written before it existed.
const usageBackfill = `
INSERT INTO usage(channel, agent, provider, time, fresh, cached, output, cost)
SELECT channel, agent,
       CASE WHEN instr(m, '/') > 0 THEN substr(m, 1, instr(m, '/') - 1) ELSE '' END,
       time, fresh, cached, output, cost
FROM (SELECT channel, agent, time,
             COALESCE(json_extract(CAST(payload AS TEXT), '$.model'), '') AS m,
             COALESCE(json_extract(CAST(payload AS TEXT), '$.usage.input_tokens'), 0) AS fresh,
             COALESCE(json_extract(CAST(payload AS TEXT), '$.usage.cache_read_tokens'), 0) AS cached,
             COALESCE(json_extract(CAST(payload AS TEXT), '$.usage.output_tokens'), 0) AS output,
             COALESCE(json_extract(CAST(payload AS TEXT), '$.cost_usd'), 0) AS cost
      FROM events WHERE type = 'assistant.message' ORDER BY global)`

// indexUsage writes a model call's row.
func indexUsage(ctx context.Context, tx *sql.Tx, e event.Event) error {
	var p event.AssistantMessagePayload
	if err := e.Decode(&p); err != nil {
		return nil // a call the fold cannot read either: it has no usage to count
	}
	provider, _, _ := strings.Cut(p.Model, "/")
	if !strings.Contains(p.Model, "/") {
		provider = ""
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO usage(channel, agent, provider, time, fresh, cached, output, cost) VALUES(?,?,?,?,?,?,?,?)`,
		e.Channel, e.Agent, provider, e.Time.UnixNano(), p.Usage.InputTokens, p.Usage.CacheReadTokens, p.Usage.OutputTokens, p.CostUSD)
	return err
}

// UsageQuery picks model calls to sum: every channel's, one channel's, or
// one agent's (Agent is an agent id within Channel), from From (the first
// call when zero) to To (now when zero), in Buckets equal spans.
type UsageQuery struct {
	Channel, Agent string
	From, To       time.Time
	Buckets        int
}

// UsageSeries is tokens and cost per bucket over [From, To).
type UsageSeries struct {
	From, To time.Time
	Tokens   []int
	Cost     []float64
}

// Usage sums the assistant messages q picks into buckets: a message's
// input and output tokens, and its cost.
func (l *Log) Usage(ctx context.Context, q UsageQuery) (UsageSeries, error) {
	where, args := `1 = 1`, []any{}
	if q.Channel != "" {
		where += ` AND channel = ?`
		args = append(args, q.Channel)
	}
	if q.Agent != "" {
		where += ` AND agent = ?`
		args = append(args, q.Agent)
	}
	to := q.To
	if to.IsZero() {
		to = time.Now()
	}
	from := q.From
	if from.IsZero() {
		var first sql.NullInt64
		if err := l.r.QueryRowContext(ctx, `SELECT MIN(time) FROM usage WHERE `+where, args...).Scan(&first); err != nil {
			return UsageSeries{}, err
		}
		from = to.Add(-time.Hour) // nothing yet: an empty hour
		if first.Valid && time.Unix(0, first.Int64).Before(from) {
			from = time.Unix(0, first.Int64)
		}
	}
	n := max(q.Buckets, 1)
	s := UsageSeries{From: from.UTC(), To: to.UTC(), Tokens: make([]int, n), Cost: make([]float64, n)}
	span := to.UnixNano() - from.UnixNano()
	if span <= 0 {
		return s, nil
	}
	rows, err := l.r.QueryContext(ctx, `
SELECT time, fresh + output, cost FROM usage WHERE `+where+` AND time >= ? AND time < ?`, append(args, from.UnixNano(), to.UnixNano())...)
	if err != nil {
		return UsageSeries{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var at int64
		var tokens int
		var cost float64
		if err := rows.Scan(&at, &tokens, &cost); err != nil {
			return UsageSeries{}, err
		}
		i := int((at - from.UnixNano()) * int64(n) / span)
		i = min(max(i, 0), n-1)
		s.Tokens[i] += tokens
		s.Cost[i] += cost
	}
	return s, rows.Err()
}

// CacheUsage sums what the model calls since from carried: tokens the
// provider charged as new input, and tokens it served from its prompt
// cache. Their ratio says whether cache routing is working
// (docs/prompt-caching.md).
// CacheUsageByProvider is CacheUsage split by the provider of each call's
// model ("openai" of "openai/gpt-…"): one provider whose cache has stopped
// working is invisible in a total that another provider's traffic dominates.
func (l *Log) CacheUsageByProvider(ctx context.Context, from time.Time) (map[string][2]int64, error) {
	rows, err := l.r.QueryContext(ctx, `SELECT provider, SUM(fresh), SUM(cached) FROM usage WHERE time >= ? AND provider != '' GROUP BY provider`, from.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][2]int64{}
	for rows.Next() {
		var provider string
		var fresh, cached int64
		if err := rows.Scan(&provider, &fresh, &cached); err != nil {
			return nil, err
		}
		out[provider] = [2]int64{fresh, cached}
	}
	return out, rows.Err()
}

func (l *Log) CacheUsage(ctx context.Context, from time.Time) (fresh, cached int64, err error) {
	row := l.r.QueryRowContext(ctx, `SELECT COALESCE(SUM(fresh), 0), COALESCE(SUM(cached), 0) FROM usage WHERE time >= ?`, from.UnixNano())
	err = row.Scan(&fresh, &cached)
	return fresh, cached, err
}
