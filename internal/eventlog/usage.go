package eventlog

import (
	"context"
	"database/sql"
	"time"
)

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
	where, args := `type = 'assistant.message'`, []any{}
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
		if err := l.r.QueryRowContext(ctx, `SELECT MIN(time) FROM events WHERE `+where, args...).Scan(&first); err != nil {
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
SELECT time,
       COALESCE(json_extract(CAST(payload AS TEXT), '$.usage.input_tokens'), 0) + COALESCE(json_extract(CAST(payload AS TEXT), '$.usage.output_tokens'), 0),
       COALESCE(json_extract(CAST(payload AS TEXT), '$.cost_usd'), 0)
FROM events WHERE `+where+` AND time >= ? AND time < ?`, append(args, from.UnixNano(), to.UnixNano())...)
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
