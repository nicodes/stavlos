package chatcompletions

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/model/stream"
)

// Complete streams one chat completion through the shared transport
// (internal/model/stream): retries before the first byte, an idle
// watchdog, and an error when the stream ends without a finish reason. On
// ctx cancellation it returns the partial accumulation with ctx.Err().
func (m *client) Complete(ctx context.Context, req model.Request, onDelta func(model.Delta)) (model.Response, error) {
	body, err := m.p.buildBody(m.id, req)
	if err != nil {
		return model.Response{}, fmt.Errorf("%s: %w", m.p.name, err)
	}
	return stream.Complete(ctx, stream.Request{
		Name: m.p.name, Client: m.p.http, URL: m.p.baseURL + "/chat/completions", Body: body, Header: m.p.header,
	}, onDelta, func(d func(model.Delta)) stream.Codec { return newAccumulator(d) })
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
	cr.MaxTokens = maxTokens
	if req.Variant != "" {
		cr.ReasoningEffort = req.Variant
	}
	return json.Marshal(cr)
}

// header sets the bearer credential from the token source.
func (p *provider) header(ctx context.Context, h http.Header) error {
	if p.token == nil {
		return nil
	}
	tok, err := p.token(ctx)
	if err != nil {
		return fmt.Errorf("%s: %w", p.name, err)
	}
	h.Set("Authorization", "Bearer "+tok.Access)
	return nil
}

// accumulator folds stream chunks into a Response.
type accumulator struct {
	onDelta  func(model.Delta)
	text     strings.Builder
	thinking strings.Builder
	calls    map[int]*callBuilder
	finish   string
	usage    model.Usage
}

// callBuilder is one tool call being streamed: its arguments arrive in
// pieces, built up without copying the whole string at every chunk.
type callBuilder struct {
	id, name string
	args     strings.Builder
}

func newAccumulator(onDelta func(model.Delta)) *accumulator {
	return &accumulator{onDelta: onDelta, calls: map[int]*callBuilder{}}
}

// Feed, Response and Terminal make the accumulator a stream.Codec.
func (a *accumulator) Feed(payload []byte) error { return a.feed(payload) }
func (a *accumulator) Response() model.Response  { return a.response() }

// Terminal: a finish reason arrived (some servers close without [DONE]).
func (a *accumulator) Terminal() bool { return a.finish != "" }

func (a *accumulator) feed(payload []byte) error {
	var chunk chatChunk
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return fmt.Errorf("decode chunk: %w", err)
	}
	if chunk.Error != nil {
		return fmt.Errorf("stream error: %s", chunk.Error.Message)
	}
	if u := chunk.Usage; u != nil {
		cached := 0
		if u.PromptTokensDetails != nil {
			cached = u.PromptTokensDetails.CachedTokens
		}
		a.usage = model.UsageFrom(u.PromptTokens, u.CompletionTokens, cached)
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
		cur = &callBuilder{}
		a.calls[tc.Index] = cur
	}
	if tc.ID != "" {
		cur.id = tc.ID
	}
	if tc.Function.Name != "" {
		if cur.name == "" {
			a.onDelta(model.Delta{ToolName: tc.Function.Name})
		}
		cur.name = tc.Function.Name
	}
	cur.args.WriteString(tc.Function.Arguments)
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
		args := strings.TrimSpace(tc.args.String())
		if args == "" || !json.Valid([]byte(args)) {
			args = "{}"
		}
		id := tc.id
		if id == "" {
			id = fmt.Sprintf("call_%d", n)
		}
		blocks = append(blocks, model.Block{Type: model.BlockToolUse, ID: id, Name: tc.name, Input: json.RawMessage(args)})
	}
	return model.Response{
		Blocks:     blocks,
		StopReason: fromFinishReason(a.finish, len(a.calls) > 0),
		Usage:      a.usage,
	}
}
