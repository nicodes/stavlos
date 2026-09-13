package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/nicodes/stavlos/internal/model"
)

// maxErrorBody bounds how much of an error response we quote.
const maxErrorBody = 2048

// Complete streams one chat completion. On ctx cancellation it returns the
// partial accumulation with ctx.Err().
func (m *client) Complete(ctx context.Context, req model.Request, onDelta func(model.Delta)) (model.Response, error) {
	if onDelta == nil {
		onDelta = func(model.Delta) {}
	}
	body, err := m.p.buildBody(m.id, req)
	if err != nil {
		return model.Response{}, fmt.Errorf("%s: %w", m.p.name, err)
	}
	resp, err := m.p.post(ctx, body)
	if err != nil {
		return model.Response{}, err
	}
	defer resp.Body.Close()

	acc := newAccumulator(onDelta)
	if err := readSSE(ctx, resp.Body, acc.feed); err != nil {
		if ctx.Err() != nil {
			return acc.response(), ctx.Err()
		}
		return acc.response(), fmt.Errorf("%s: %w", m.p.name, err)
	}
	return acc.response(), nil
}

func (p *provider) buildBody(id string, req model.Request) ([]byte, error) {
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}
	cr := chatRequest{
		Model:         id,
		Messages:      toMessages(req.System, req.Messages),
		Tools:         toTools(req.Tools),
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
	}
	if p.usesMaxCompletionTokens() {
		cr.MaxCompletionTokens = maxTokens
	} else {
		cr.MaxTokens = maxTokens
	}
	if req.Variant != "" {
		cr.ReasoningEffort = req.Variant
	}
	return json.Marshal(cr)
}

func (p *provider) post(ctx context.Context, body []byte) (*http.Response, error) {
	url := p.baseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.name, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	switch {
	case p.token != nil:
		tok, err := p.token(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.name, err)
		}
		httpReq.Header.Set("Authorization", "Bearer "+tok.Access)
	case p.apiKey != "":
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := p.http.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s: %w", p.name, err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, fmt.Errorf("%s: status %d: %s", p.name, resp.StatusCode, errorText(raw))
	}
	return resp, nil
}

// errorText extracts the API's message from an error body, or quotes it.
func errorText(raw []byte) string {
	var env errorEnvelope
	if json.Unmarshal(raw, &env) == nil && env.Error != nil && env.Error.Message != "" {
		if env.Error.Type != "" {
			return env.Error.Type + ": " + env.Error.Message
		}
		return env.Error.Message
	}
	return strings.TrimSpace(string(raw))
}

// readSSE parses a text/event-stream body, calling onData for each
// "data:" payload until [DONE], EOF, or ctx cancellation.
func readSSE(ctx context.Context, r io.Reader, onData func([]byte) error) error {
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
		if err := ctx.Err(); err != nil {
			return err
		}
		line := sc.Bytes()
		switch {
		case len(line) == 0:
			if err := flush(); err != nil {
				if err == io.EOF {
					return nil
				}
				return err
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
			return ctx.Err()
		}
		return err
	}
	if err := flush(); err != nil && err != io.EOF {
		return err
	}
	return nil
}

// accumulator folds stream chunks into a Response.
type accumulator struct {
	onDelta  func(model.Delta)
	text     strings.Builder
	thinking strings.Builder
	calls    map[int]*toolCall
	finish   string
	usage    model.Usage
	sawUsage bool
}

func newAccumulator(onDelta func(model.Delta)) *accumulator {
	return &accumulator{onDelta: onDelta, calls: map[int]*toolCall{}}
}

func (a *accumulator) feed(payload []byte) error {
	var chunk chatChunk
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return fmt.Errorf("decode chunk: %w", err)
	}
	if chunk.Error != nil {
		return fmt.Errorf("stream error: %s", chunk.Error.Message)
	}
	if chunk.Usage != nil {
		a.sawUsage = true
		a.usage.InputTokens = chunk.Usage.PromptTokens
		a.usage.OutputTokens = chunk.Usage.CompletionTokens
		if d := chunk.Usage.PromptTokensDetails; d != nil {
			a.usage.CacheReadTokens = d.CachedTokens
			a.usage.InputTokens -= d.CachedTokens
			if a.usage.InputTokens < 0 {
				a.usage.InputTokens = 0
			}
		}
	}
	for _, ch := range chunk.Choices {
		if ch.Index != 0 {
			continue
		}
		d := ch.Delta
		if d.Content != nil && *d.Content != "" {
			a.text.WriteString(*d.Content)
			a.onDelta(model.Delta{Text: *d.Content})
		}
		reasoning := d.ReasoningContent
		if reasoning == nil {
			reasoning = d.Reasoning
		}
		if reasoning != nil && *reasoning != "" {
			a.thinking.WriteString(*reasoning)
			a.onDelta(model.Delta{Thinking: *reasoning})
		}
		for _, tc := range d.ToolCalls {
			a.feedToolCall(tc)
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			a.finish = *ch.FinishReason
		}
	}
	return nil
}

func (a *accumulator) feedToolCall(tc toolCall) {
	cur, ok := a.calls[tc.Index]
	if !ok {
		cur = &toolCall{Index: tc.Index}
		a.calls[tc.Index] = cur
	}
	if tc.ID != "" {
		cur.ID = tc.ID
	}
	if tc.Function.Name != "" {
		if cur.Function.Name == "" {
			a.onDelta(model.Delta{ToolName: tc.Function.Name})
		}
		cur.Function.Name = tc.Function.Name
	}
	cur.Function.Arguments += tc.Function.Arguments
}

func (a *accumulator) response() model.Response {
	var blocks []model.Block
	if a.thinking.Len() > 0 {
		blocks = append(blocks, model.Block{Type: model.BlockThinking, Text: a.thinking.String()})
	}
	if a.text.Len() > 0 {
		blocks = append(blocks, model.Block{Type: model.BlockText, Text: a.text.String()})
	}
	idx := make([]int, 0, len(a.calls))
	for i := range a.calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for n, i := range idx {
		tc := a.calls[i]
		args := strings.TrimSpace(tc.Function.Arguments)
		if args == "" || !json.Valid([]byte(args)) {
			args = "{}"
		}
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", n)
		}
		blocks = append(blocks, model.Block{
			Type:  model.BlockToolUse,
			ID:    id,
			Name:  tc.Function.Name,
			Input: json.RawMessage(args),
		})
	}
	return model.Response{
		Blocks:     blocks,
		StopReason: fromFinishReason(a.finish, len(a.calls) > 0),
		Usage:      a.usage,
	}
}
