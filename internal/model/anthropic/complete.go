package anthropic

import (
	"context"
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/nicodes/stavlos/internal/model"
)

// Complete streams one call, forwarding deltas and returning the accumulated
// message. On ctx cancellation it returns the partial blocks with ctx.Err().
func (m *client) Complete(ctx context.Context, req model.Request, onDelta func(model.Delta)) (model.Response, error) {
	if onDelta == nil {
		onDelta = func(model.Delta) {}
	}
	params, err := buildParams(req)
	if err != nil {
		return model.Response{}, fmt.Errorf("anthropic: %w", err)
	}

	stream := m.c.Messages.NewStreaming(ctx, params)
	defer stream.Close()

	acc := anthropic.Message{}
	for stream.Next() {
		if ctx.Err() != nil {
			break
		}
		ev := stream.Current()
		if err := acc.Accumulate(ev); err != nil {
			return partial(acc), fmt.Errorf("anthropic: accumulate: %w", err)
		}
		switch e := ev.AsAny().(type) {
		case anthropic.ContentBlockStartEvent:
			if e.ContentBlock.Type == "tool_use" {
				onDelta(model.Delta{ToolName: e.ContentBlock.Name})
			}
		case anthropic.ContentBlockDeltaEvent:
			switch d := e.Delta.AsAny().(type) {
			case anthropic.TextDelta:
				onDelta(model.Delta{Text: d.Text})
			case anthropic.ThinkingDelta:
				onDelta(model.Delta{Thinking: d.Thinking})
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return partial(acc), err
	}
	if err := stream.Err(); err != nil {
		return partial(acc), wrapErr(err)
	}
	return model.Response{
		Blocks:     fromContent(acc.Content),
		StopReason: fromStopReason(acc.StopReason),
		Usage:      fromUsage(acc.Usage),
	}, nil
}

// partial packages whatever was accumulated before a failure.
func partial(acc anthropic.Message) model.Response {
	return model.Response{
		Blocks:     fromContent(acc.Content),
		StopReason: model.StopOther,
		Usage:      fromUsage(acc.Usage),
	}
}

// wrapErr annotates API errors with their HTTP status and error type.
func wrapErr(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return fmt.Errorf("anthropic: status %d (%s): %w", apiErr.StatusCode, apiErr.Type(), err)
	}
	return fmt.Errorf("anthropic: %w", err)
}
