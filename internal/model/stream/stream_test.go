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
	return Complete(ctx, Request{Name: "test", URL: url, Body: []byte(`{}`)}, c)
}

func TestEndings(t *testing.T) {
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
		}}, &codec{})
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
		}}, &codec{})
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
