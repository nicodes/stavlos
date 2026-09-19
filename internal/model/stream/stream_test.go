package stream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/model"
)

func init() { RetryDelay = time.Millisecond }

// codec collects payloads as text; "end" is its terminal event, "fin" sets
// Terminal without stopping.
type codec struct {
	mu   sync.Mutex
	text strings.Builder
	term bool
}

func (c *codec) Feed(p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch string(p) {
	case "end":
		c.term = true
		return io.EOF
	case "fin":
		c.term = true
		return nil
	case "boom":
		return errors.New("bad event")
	}
	c.text.WriteString(string(p))
	return nil
}

func (c *codec) Response() model.Response {
	c.mu.Lock()
	defer c.mu.Unlock()
	return model.Response{Blocks: []model.Block{{Type: model.BlockText, Text: c.text.String()}}, StopReason: model.StopEndTurn}
}

func (c *codec) Terminal() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.term
}

func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func call(ctx context.Context, url string, c *codec) (model.Response, error) {
	return Complete(ctx, Request{Name: "test", URL: url, Body: []byte(`{}`)}, nil, same(c))
}

// same is a codec factory handing out one codec for every attempt.
func same(c Codec) func(func(model.Delta)) Codec {
	return func(func(model.Delta)) Codec { return c }
}

// noRetry turns mid-stream retries off for a test that pins how one attempt
// ends.
func noRetry(t *testing.T) {
	old := MidStreamRetries
	MidStreamRetries = 0
	t.Cleanup(func() { MidStreamRetries = old })
}

func TestEndings(t *testing.T) {
	noRetry(t)
	cases := []struct {
		name, body string
		wantErr    string // "" = none
		wantText   string
		wantStop   model.StopReason
	}{
		{"terminal event", "data: a\n\ndata: end\n\ndata: ignored\n\n", "", "a", model.StopEndTurn},
		{"done marker", "data: a\n\ndata: [DONE]\n\n", "", "a", model.StopEndTurn},
		{"finish then close", "data: a\n\ndata: fin\n\n", "", "a", model.StopEndTurn},
		{"closed early", "data: a\n\ndata: b\n\n", "stream ended before the response was complete", "ab", model.StopOther},
		{"closed mid-event", "data: a\n\ndata: b", "stream ended before the response was complete", "ab", model.StopOther},
		{"stream error", "data: a\n\ndata: boom\n\n", "test: bad event", "a", model.StopEndTurn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tc.body) })
			res, err := call(context.Background(), srv.URL, &codec{})
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if res.Blocks[0].Text != tc.wantText || res.StopReason != tc.wantStop {
				t.Fatalf("response %+v", res)
			}
		})
	}
	if _, err := call(context.Background(), serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "data: a\n\n") }).URL, &codec{}); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("errors.Is ErrIncomplete: %v", err)
	}
}

func TestRetries(t *testing.T) {
	t.Run("retryable statuses then success", func(t *testing.T) {
		var hits atomic.Int32
		srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			switch hits.Add(1) {
			case 1:
				w.Header().Set("Retry-After", "0")
				http.Error(w, `{"error":{"message":"busy"}}`, http.StatusServiceUnavailable)
			case 2:
				http.Error(w, `{"error":{"message":"slow down"}}`, http.StatusTooManyRequests)
			default:
				_, _ = io.WriteString(w, "data: ok\n\ndata: end\n\n")
			}
		})
		res, err := call(context.Background(), srv.URL, &codec{})
		if err != nil || res.Blocks[0].Text != "ok" || hits.Load() != 3 {
			t.Fatalf("res %+v err %v hits %d", res, err, hits.Load())
		}
	})
	t.Run("gives up after MaxAttempts", func(t *testing.T) {
		var hits atomic.Int32
		srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			http.Error(w, `{"error":{"type":"rate_limit","message":"slow down"}}`, http.StatusTooManyRequests)
		})
		_, err := call(context.Background(), srv.URL, &codec{})
		if err == nil || err.Error() != "test: status 429: rate_limit: slow down" || int(hits.Load()) != MaxAttempts {
			t.Fatalf("err %v hits %d", err, hits.Load())
		}
	})
	t.Run("a client error is not retried", func(t *testing.T) {
		var hits atomic.Int32
		srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			http.Error(w, `{"error":{"message":"bad request"}}`, http.StatusBadRequest)
		})
		_, err := call(context.Background(), srv.URL, &codec{})
		if err == nil || !strings.Contains(err.Error(), "status 400: bad request") || hits.Load() != 1 {
			t.Fatalf("err %v hits %d", err, hits.Load())
		}
	})
	t.Run("a provider's status error wins and is not retried", func(t *testing.T) {
		var hits atomic.Int32
		srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
		})
		_, err := Complete(context.Background(), Request{Name: "test", URL: srv.URL, OnStatus: func(code int, _ []byte) error {
			if code == http.StatusUnauthorized {
				return errors.New("log in again")
			}
			return nil
		}}, nil, same(&codec{}))
		if err == nil || err.Error() != "log in again" || hits.Load() != 1 {
			t.Fatalf("err %v hits %d", err, hits.Load())
		}
	})
	t.Run("headers run on every attempt", func(t *testing.T) {
		var hits, headers atomic.Int32
		srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer t" || r.Header.Get("Accept") != "text/event-stream" {
				t.Errorf("headers %v", r.Header)
			}
			if hits.Add(1) == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_, _ = io.WriteString(w, "data: end\n\n")
		})
		_, err := Complete(context.Background(), Request{Name: "test", URL: srv.URL, Header: func(_ context.Context, h http.Header) error {
			headers.Add(1)
			h.Set("Authorization", "Bearer t")
			return nil
		}}, nil, same(&codec{}))
		if err != nil || headers.Load() != 2 {
			t.Fatalf("err %v header calls %d", err, headers.Load())
		}
	})
	if retryAfter("7") != 7*time.Second || retryAfter("") != 0 || retryAfter("soon") != 0 || retryAfter(time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)) != 0 {
		t.Fatal("retryAfter")
	}
}

func TestIdleAndCancel(t *testing.T) {
	stall := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "data: partial\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}
	t.Run("idle", func(t *testing.T) {
		noRetry(t)
		old := IdleTimeout
		IdleTimeout = 150 * time.Millisecond
		defer func() { IdleTimeout = old }()
		res, err := call(context.Background(), serve(t, stall).URL, &codec{})
		if err == nil || !strings.Contains(err.Error(), "no data from the provider") || res.Blocks[0].Text != "partial" || res.StopReason != model.StopOther {
			t.Fatalf("res %+v err %v", res, err)
		}
	})
	t.Run("cancel keeps the partial", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		c := &codec{}
		go func() {
			for c.Response().Blocks[0].Text == "" {
				time.Sleep(5 * time.Millisecond)
			}
			cancel()
		}()
		res, err := call(ctx, serve(t, stall).URL, c)
		if !errors.Is(err, context.Canceled) || res.Blocks[0].Text != "partial" {
			t.Fatalf("res %+v err %v", res, err)
		}
	})
}

// streamingCodec is codec streaming each payload as text ("tool" as a tool
// call starting) through the delta callback of its attempt.
type streamingCodec struct {
	codec
	onDelta func(model.Delta)
}

func (c *streamingCodec) Feed(p []byte) error {
	switch string(p) {
	case "tool":
		c.onDelta(model.Delta{ToolName: "shell"})
	case "end", "fin", "boom":
	default:
		c.onDelta(model.Delta{Text: string(p)})
	}
	return c.codec.Feed(p)
}

// TestMidStreamRetry: a stream that breaks off is sent again once, after a
// Reset delta, with a fresh codec; not when a tool call had begun, and not
// when the provider itself failed the stream.
func TestMidStreamRetry(t *testing.T) {
	run := func(t *testing.T, first string) (model.Response, []model.Delta, int32, error) {
		var hits atomic.Int32
		srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			if hits.Add(1) == 1 {
				_, _ = io.WriteString(w, first) // then the response ends without a terminal event
				return
			}
			_, _ = io.WriteString(w, "data: ok\n\ndata: end\n\n")
		})
		var mu sync.Mutex
		var deltas []model.Delta
		res, err := Complete(context.Background(), Request{Name: "test", URL: srv.URL, Body: []byte(`{}`)},
			func(d model.Delta) { mu.Lock(); deltas = append(deltas, d); mu.Unlock() },
			func(onDelta func(model.Delta)) Codec { return &streamingCodec{onDelta: onDelta} })
		return res, deltas, hits.Load(), err
	}
	t.Run("broken stream retried", func(t *testing.T) {
		res, deltas, hits, err := run(t, "data: par\n\n")
		if err != nil || res.Blocks[0].Text != "ok" || hits != 2 {
			t.Fatalf("res %+v err %v hits %d", res, err, hits)
		}
		if len(deltas) != 3 || deltas[0].Text != "par" || !deltas[1].Reset || deltas[2].Text != "ok" {
			t.Fatalf("deltas %+v", deltas)
		}
	})
	t.Run("not after a tool call began", func(t *testing.T) {
		_, _, hits, err := run(t, "data: tool\n\n")
		if !errors.Is(err, ErrIncomplete) || hits != 1 {
			t.Fatalf("err %v hits %d", err, hits)
		}
	})
	t.Run("not when the provider failed the stream", func(t *testing.T) {
		_, _, hits, err := run(t, "data: boom\n\n")
		if err == nil || !strings.Contains(err.Error(), "bad event") || hits != 1 {
			t.Fatalf("err %v hits %d", err, hits)
		}
	})
}

// TestPlanLimitRefusals: a used-up plan is refused in whatever status its
// provider likes, and every one of them must come out as a model.LimitError,
// at once, so the agent runtime moves the agent to another model inside the
// turn. The xAI body is the one a SuperGrok account out of credits got on
// 2026-09-18: a 403, which ended six turns with an error because only a 429
// counted.
func TestPlanLimitRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		limit  bool
		hits   int32
	}{
		{"xai out of credits", http.StatusForbidden, `{"code":"personal-team-blocked:spending-limit","error":"You have run out of credits or need a Grok subscription. Add credits at https://grok.com/?_s=usage"}`, true, 1},
		{"codex usage limit", http.StatusTooManyRequests, `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":4000}}`, true, 1},
		{"a balance that ran out, as a 402", http.StatusPaymentRequired, `{"error":{"code":"1113","message":"Insufficient balance or no resource package. Please recharge."}}`, true, 1},
		{"openai-style quota", http.StatusTooManyRequests, `{"error":{"type":"insufficient_quota","message":"You exceeded your current quota"}}`, true, 1},
		{"a 403 that means forbidden", http.StatusForbidden, `{"error":{"message":"insufficient permissions for this model"}}`, false, 1},
		{"a burst: retried, and a limit only once it outlasts the retries", http.StatusTooManyRequests, `{"error":{"type":"rate_limit","message":"slow down"}}`, true, int32(MaxAttempts)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				http.Error(w, tc.body, tc.status)
			})
			_, err := call(context.Background(), srv.URL, &codec{})
			var le *model.LimitError
			if err == nil || errors.As(err, &le) != tc.limit || hits.Load() != tc.hits {
				t.Fatalf("err %v, a limit: %v (want %v), attempts %d (want %d)", err, errors.As(err, &le), tc.limit, hits.Load(), tc.hits)
			}
		})
	}
}
