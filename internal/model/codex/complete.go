package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/nicodes/stavlos/internal/model"
)

// Complete streams one Responses call. On ctx cancellation it returns the
// partial accumulation with ctx.Err().
func (m *client) Complete(ctx context.Context, req model.Request, onDelta func(model.Delta)) (model.Response, error) {
	if onDelta == nil {
		onDelta = func(model.Delta) {}
	}
	body, err := buildBody(m.id, req)
	if err != nil {
		return model.Response{}, fmt.Errorf("codex: %w", err)
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
		return acc.response(), fmt.Errorf("codex: %w", err)
	}
	return acc.response(), nil
}

// readSSE parses a text/event-stream body, calling onData for each
// "data:" payload until [DONE], EOF, ctx cancellation, or onData returning
// errStreamDone (a terminal event was seen).
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

// event is the union of stream event shapes we care about.
type event struct {
	Type        string       `json:"type"`
	OutputIndex *int         `json:"output_index"`
	Delta       string       `json:"delta"`
	Arguments   string       `json:"arguments"` // function_call_arguments.done
	Item        *outputItem  `json:"item"`
	Response    *responseObj `json:"response"`
	Error       *apiError    `json:"error"`
	Message     string       `json:"message"` // some error events are flat
}

type apiError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) String() string {
	if e == nil {
		return ""
	}
	if e.Type != "" {
		return e.Type + ": " + e.Message
	}
	return e.Message
}

// outputItem is a Responses output item (in output_item.* events and in
// the final response.output list).
type outputItem struct {
	Type             string        `json:"type"`
	ID               string        `json:"id"`
	Role             string        `json:"role"`
	Content          []contentPart `json:"content"`
	EncryptedContent string        `json:"encrypted_content"`
	CallID           string        `json:"call_id"`
	Name             string        `json:"name"`
	Arguments        string        `json:"arguments"`
	Summary          []contentPart `json:"summary"`
}

type responseObj struct {
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []outputItem `json:"output"`
	Usage  *struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		InputTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
	Error *apiError `json:"error"`
}

// item is one in-flight output item, keyed by output_index.
type item struct {
	kind string // "message", "reasoning", "function_call"
	id   string
	name string
	text strings.Builder // message text or reasoning summary
	args strings.Builder // streamed function_call arguments
	// argsFinal is set by function_call_arguments.done and wins over args.
	argsFinal *string
	signature string
	announced bool // ToolName delta already sent
}

// accumulator folds stream events into a Response, preserving item order.
type accumulator struct {
	onDelta    func(model.Delta)
	items      map[int]*item
	order      []int // output indexes in first-seen order
	usage      model.Usage
	status     string
	incomplete string
	final      []outputItem // response.output from response.completed
}

func newAccumulator(onDelta func(model.Delta)) *accumulator {
	return &accumulator{onDelta: onDelta, items: map[int]*item{}}
}

// at returns the item at output index idx, creating it with kind if new.
// A nil idx addresses the most recently opened item.
func (a *accumulator) at(idx *int, kind string) *item {
	var i int
	switch {
	case idx != nil:
		i = *idx
	case len(a.order) > 0:
		i = a.order[len(a.order)-1]
	default:
		i = 0
	}
	it, ok := a.items[i]
	if !ok {
		it = &item{kind: kind}
		a.items[i] = it
		a.order = append(a.order, i)
	}
	if it.kind == "" {
		it.kind = kind
	}
	return it
}

func (a *accumulator) feed(payload []byte) error {
	var ev event
	if err := json.Unmarshal(payload, &ev); err != nil {
		return fmt.Errorf("decode event: %w", err)
	}
	switch ev.Type {
	case "response.output_text.delta":
		if ev.Delta == "" {
			return nil
		}
		a.at(ev.OutputIndex, "message").text.WriteString(ev.Delta)
		a.onDelta(model.Delta{Text: ev.Delta})

	case "response.reasoning_summary_text.delta":
		if ev.Delta == "" {
			return nil
		}
		a.at(ev.OutputIndex, "reasoning").text.WriteString(ev.Delta)
		a.onDelta(model.Delta{Thinking: ev.Delta})

	case "response.output_item.added":
		if ev.Item == nil {
			return nil
		}
		it := a.at(ev.OutputIndex, ev.Item.Type)
		a.applyItem(it, ev.Item, false)

	case "response.function_call_arguments.delta":
		a.at(ev.OutputIndex, "function_call").args.WriteString(ev.Delta)

	case "response.function_call_arguments.done":
		it := a.at(ev.OutputIndex, "function_call")
		if ev.Arguments != "" {
			s := ev.Arguments
			it.argsFinal = &s
		}

	case "response.output_item.done":
		if ev.Item == nil {
			return nil
		}
		it := a.at(ev.OutputIndex, ev.Item.Type)
		a.applyItem(it, ev.Item, true)

	case "response.completed", "response.incomplete":
		if r := ev.Response; r != nil {
			a.status = r.Status
			if r.IncompleteDetails != nil {
				a.incomplete = r.IncompleteDetails.Reason
			}
			if ev.Type == "response.incomplete" && a.status == "" {
				a.status = "incomplete"
			}
			if u := r.Usage; u != nil {
				cached := 0
				if u.InputTokensDetails != nil {
					cached = u.InputTokensDetails.CachedTokens
				}
				a.usage.InputTokens = u.InputTokens - cached
				if a.usage.InputTokens < 0 {
					a.usage.InputTokens = 0
				}
				a.usage.CacheReadTokens = cached
				a.usage.OutputTokens = u.OutputTokens
			}
			a.final = r.Output
		}
		return io.EOF // terminal: stop reading

	case "response.failed":
		msg := "response failed"
		if ev.Response != nil && ev.Response.Error != nil {
			msg = ev.Response.Error.String()
		} else if ev.Error != nil {
			msg = ev.Error.String()
		}
		return fmt.Errorf("stream error: %s", msg)

	case "error":
		msg := ev.Message
		if ev.Error != nil {
			msg = ev.Error.String()
		}
		if msg == "" {
			msg = "unknown error"
		}
		return fmt.Errorf("stream error: %s", msg)
	}
	return nil
}

// applyItem merges an output item's fields into it. done marks the
// output_item.done event, where the item carries final values.
func (a *accumulator) applyItem(it *item, oi *outputItem, done bool) {
	if oi.Type != "" {
		it.kind = oi.Type
	}
	switch oi.Type {
	case "function_call":
		if oi.CallID != "" {
			it.id = oi.CallID
		}
		if oi.Name != "" {
			it.name = oi.Name
			if !it.announced {
				it.announced = true
				a.onDelta(model.Delta{ToolName: oi.Name})
			}
		}
		if done && oi.Arguments != "" && it.argsFinal == nil && it.args.Len() == 0 {
			s := oi.Arguments
			it.argsFinal = &s
		}
	case "reasoning":
		if oi.ID != "" {
			it.id = oi.ID
		}
		if oi.EncryptedContent != "" {
			it.signature = oi.EncryptedContent
		}
		if done && it.text.Len() == 0 {
			for _, p := range oi.Summary {
				it.text.WriteString(p.Text)
			}
		}
	case "message":
		if oi.ID != "" {
			it.id = oi.ID
		}
		if done && it.text.Len() == 0 {
			for _, p := range oi.Content {
				it.text.WriteString(p.Text)
			}
		}
	}
}

func (a *accumulator) response() model.Response {
	var blocks []model.Block
	for _, i := range a.order {
		if b, ok := a.items[i].block(); ok {
			blocks = append(blocks, b)
		}
	}
	if len(blocks) == 0 && len(a.final) > 0 {
		// Safety net: rebuild from the final response.output list.
		for _, oi := range a.final {
			it := &item{}
			a.applyItem(it, &oi, true)
			if b, ok := it.block(); ok {
				blocks = append(blocks, b)
			}
		}
	}
	hasTool := false
	for _, b := range blocks {
		if b.Type == model.BlockToolUse {
			hasTool = true
			break
		}
	}
	stop := model.StopEndTurn
	switch {
	case a.status == "incomplete" && a.incomplete == "max_output_tokens":
		stop = model.StopMaxTokens
	case hasTool:
		stop = model.StopToolUse
	}
	return model.Response{Blocks: blocks, StopReason: stop, Usage: a.usage}
}

// block renders the item as a model.Block; ok is false for empty items.
func (it *item) block() (model.Block, bool) {
	switch it.kind {
	case "message":
		if it.text.Len() == 0 {
			return model.Block{}, false
		}
		return model.Block{Type: model.BlockText, Text: it.text.String()}, true
	case "reasoning":
		if it.text.Len() == 0 && it.signature == "" {
			return model.Block{}, false
		}
		return model.Block{
			Type:      model.BlockThinking,
			ID:        it.id,
			Text:      it.text.String(),
			Signature: it.signature,
		}, true
	case "function_call":
		if it.name == "" && it.id == "" {
			return model.Block{}, false
		}
		args := it.args.String()
		if it.argsFinal != nil {
			args = *it.argsFinal
		}
		args = strings.TrimSpace(args)
		if args == "" || !json.Valid([]byte(args)) {
			args = "{}"
		}
		return model.Block{
			Type:  model.BlockToolUse,
			ID:    it.id,
			Name:  it.name,
			Input: json.RawMessage(args),
		}, true
	}
	return model.Block{}, false
}
