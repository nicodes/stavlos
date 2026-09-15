// Package stream is the transport every streaming model adapter shares:
// POST a request body, retry what is worth retrying before the first byte,
// read the server-sent events, feed each payload to the adapter's codec,
// and decide how the call ended. An adapter only builds its body, sets its
// headers and folds its own event shapes into a model.Response.
package stream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nicodes/stavlos/internal/model"
)

// Codec folds one provider's stream into a model.Response.
type Codec interface {
	// Feed takes one "data:" payload. io.EOF means a terminal event was
	// seen and reading stops; any other error ends the call.
	Feed(payload []byte) error
	// Response is what has been accumulated so far (partial on error).
	Response() model.Response
	// Terminal reports whether the provider said the response was complete
	// (a finish reason, a completed event).
	Terminal() bool
}

// Request describes one streaming call.
type Request struct {
	Name   string       // error prefix: "codex", "xai"
	Client *http.Client // NewHTTPClient when nil
	URL    string
	Body   []byte
	// Header sets auth and provider headers; it runs for every attempt, so
	// a rotated token is picked up on a retry.
	Header func(ctx context.Context, h http.Header) error
	// OnStatus may turn a non-2xx status into a provider-specific error
	// (e.g. "log in again" for 401); nil or a nil return keeps the default.
	OnStatus func(code int, body []byte) error
}

// Tunables; variables so tests can shrink them.
var (
	MaxAttempts = 3                      // attempts before the first byte, for retryable failures
	RetryDelay  = 500 * time.Millisecond // first backoff; doubles, jittered, capped at maxRetryWait
	IdleTimeout = 2 * time.Minute        // no bytes for this long ends the call
	// MidStreamRetries is how many times a call whose stream broke off
	// (the connection dropped, went idle, or ended without a terminal event)
	// is sent again, as long as no tool call had begun streaming.
	MidStreamRetries = 1
	maxRetryWait     = 30 * time.Second
	maxErrorBody     = 2048
)

// ErrIncomplete: the stream ended cleanly but without the provider saying
// the response was complete (a closed connection, a proxy cutting it).
var ErrIncomplete = errors.New("stream ended before the response was complete")

// NewHTTPClient is the client adapters use: no overall timeout (streams are
// long and the context bounds them), three minutes for the first response
// header, proxies from the environment.
func NewHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: 3 * time.Minute,
	}}
}

// Complete runs one streaming call. newCodec makes the codec for an
// attempt, streaming through the delta callback it is given. A stream that
// breaks off before a tool call began is sent again once, after a Reset
// delta tells the caller to drop what streamed. On cancellation it returns
// the partial response with ctx.Err(); on any other failure the partial
// response and a "name: …" error.
func Complete(ctx context.Context, r Request, onDelta func(model.Delta), newCodec func(onDelta func(model.Delta)) Codec) (model.Response, error) {
	if r.Client == nil {
		r.Client = NewHTTPClient()
	}
	if onDelta == nil {
		onDelta = func(model.Delta) {}
	}
	for attempt := 0; ; attempt++ {
		var streamed, tool bool
		codec := newCodec(func(d model.Delta) {
			streamed = true
			tool = tool || d.ToolName != ""
			onDelta(d)
		})
		res, broke, err := attemptStream(ctx, r, codec)
		if err == nil || !broke || tool || attempt >= MidStreamRetries || ctx.Err() != nil {
			return res, err
		}
		if streamed {
			onDelta(model.Delta{Reset: true})
		}
	}
}

// attemptStream is one call; broke reports that its stream broke off after
// it began (a dropped connection, an idle timeout, a missing terminal
// event), as opposed to the provider rejecting the call or saying the
// stream failed.
func attemptStream(ctx context.Context, r Request, codec Codec) (res model.Response, broke bool, err error) {
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	resp, err := post(reqCtx, r)
	if err != nil {
		if ctx.Err() != nil {
			return model.Response{}, false, ctx.Err()
		}
		return model.Response{}, false, err
	}
	defer resp.Body.Close()

	var idle atomic.Bool
	watchdog := time.AfterFunc(IdleTimeout, func() {
		idle.Store(true)
		cancel()
	})
	defer watchdog.Stop()
	var codecFailed bool
	feed := func(p []byte) error {
		err := codec.Feed(p)
		codecFailed = err != nil && !errors.Is(err, io.EOF)
		return err
	}
	done, err := ReadSSE(reqCtx, resp.Body, feed, func() { watchdog.Reset(IdleTimeout) })
	switch {
	case ctx.Err() != nil:
		return codec.Response(), false, ctx.Err()
	case idle.Load():
		return incomplete(codec), true, fmt.Errorf("%s: no data from the provider for %s", r.Name, IdleTimeout)
	case err != nil:
		return codec.Response(), !codecFailed, fmt.Errorf("%s: %w", r.Name, err)
	case !done && !codec.Terminal():
		return incomplete(codec), true, fmt.Errorf("%s: %w", r.Name, ErrIncomplete)
	}
	return codec.Response(), false, nil
}

func incomplete(codec Codec) model.Response {
	res := codec.Response()
	res.StopReason = model.StopOther
	return res
}

// post sends the body, retrying transport failures and 408, 409, 429 and
// 5xx responses with jittered exponential backoff (honouring Retry-After).
// Nothing has been streamed yet, so a retry cannot duplicate output.
func post(ctx context.Context, r Request) (*http.Response, error) {
	delay := RetryDelay
	for attempt := 1; ; attempt++ {
		resp, err := attemptOnce(ctx, r)
		if err == nil {
			return resp, nil
		}
		var re *retryable
		if !errors.As(err, &re) || attempt >= MaxAttempts || ctx.Err() != nil {
			if re != nil {
				return nil, re.err
			}
			return nil, err
		}
		wait := delay/2 + time.Duration(rand.Int64N(int64(delay)+1))
		if re.after > wait {
			wait = re.after
		}
		wait = min(wait, maxRetryWait)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		delay *= 2
	}
}

type retryable struct {
	err   error
	after time.Duration // the server's Retry-After, if any
}

func (e *retryable) Error() string { return e.err.Error() }
func (e *retryable) Unwrap() error { return e.err }

func attemptOnce(ctx context.Context, r Request) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL, bytes.NewReader(r.Body))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", r.Name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if r.Header != nil {
		if err := r.Header(ctx, req.Header); err != nil {
			return nil, err
		}
	}
	resp, err := r.Client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &retryable{err: fmt.Errorf("%s: %w", r.Name, err)}
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, int64(maxErrorBody)))
	if r.OnStatus != nil {
		if err := r.OnStatus(resp.StatusCode, raw); err != nil {
			return nil, err
		}
	}
	err = fmt.Errorf("%s: status %d: %s", r.Name, resp.StatusCode, ErrorText(raw))
	switch c := resp.StatusCode; {
	case c == http.StatusRequestTimeout, c == http.StatusConflict, c == http.StatusTooManyRequests, c >= 500:
		return nil, &retryable{err: err, after: retryAfter(resp.Header.Get("Retry-After"))}
	}
	return nil, err
}

// retryAfter parses a Retry-After header: seconds or an HTTP date.
func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0)
	}
	return 0
}

// ErrorText extracts the API's message from an error body shaped
// {"error": {"type", "message"}} (OpenAI, xAI, Codex), or quotes the body.
func ErrorText(raw []byte) string {
	var env struct {
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &env) == nil && env.Error != nil && env.Error.Message != "" {
		if env.Error.Type != "" {
			return env.Error.Type + ": " + env.Error.Message
		}
		return env.Error.Message
	}
	return strings.TrimSpace(string(raw))
}

// ReadSSE parses a text/event-stream body, calling onData for each "data:"
// payload and onLine (may be nil) for every line read. done reports that the
// stream ended on purpose: a [DONE] payload, or onData returning io.EOF.
func ReadSSE(ctx context.Context, r io.Reader, onData func([]byte) error, onLine func()) (done bool, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	var data []byte
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := data
		data = nil
		if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
			return io.EOF
		}
		return onData(payload)
	}
	for sc.Scan() {
		if onLine != nil {
			onLine()
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		line := sc.Bytes()
		switch {
		case len(line) == 0:
			if err := flush(); err != nil {
				if err == io.EOF {
					return true, nil
				}
				return false, err
			}
		case bytes.HasPrefix(line, []byte(":")):
			// comment / keepalive
		case bytes.HasPrefix(line, []byte("data:")):
			v := bytes.TrimPrefix(line, []byte("data:"))
			v = bytes.TrimPrefix(v, []byte(" "))
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, v...)
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, err
	}
	if err := flush(); err != nil {
		if err == io.EOF {
			return true, nil
		}
		return false, err
	}
	return false, nil
}
