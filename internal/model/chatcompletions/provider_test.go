package chatcompletions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/model"
)

const cannedStream = `data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":"thinking..."},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"read_file","arguments":"{\"pa"}}]},"finish_reason":null}]}

: keepalive

data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a.go\"}"}}]},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"c1","choices":[],"usage":{"prompt_tokens":120,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":100}}}

data: [DONE]

`

func TestCompleteStream(t *testing.T) {
	var gotReq chatRequest
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, cannedStream)
	}))
	defer srv.Close()

	p := NewWithToken("test", srv.URL+"/v1/", token("sk-test"), Traits{})
	m, err := p.Open("some-model")
	if err != nil {
		t.Fatal(err)
	}

	var deltas []model.Delta
	req := model.Request{
		Model:  "some-model",
		System: "sys",
		Messages: []model.Message{
			{Role: model.RoleUser, Blocks: []model.Block{{Type: model.BlockText, Text: "hi"}}},
			{Role: model.RoleUser, Blocks: []model.Block{{Type: model.BlockText, Text: "again"}}},
			{Role: model.RoleAssistant, Blocks: []model.Block{
				{Type: model.BlockText, Text: "calling"},
				{Type: model.BlockToolUse, ID: "call_0", Name: "ls", Input: json.RawMessage(`{"p":"."}`)},
			}},
			{Role: model.RoleUser, Blocks: []model.Block{
				{Type: model.BlockToolResult, ToolUseID: "call_0", Content: "a.go"},
				{Type: model.BlockText, Text: "now read it"},
			}},
		},
		Tools: []model.ToolDef{{Name: "read_file", Description: "read", Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}},
	}
	resp, err := m.Complete(context.Background(), req, func(d model.Delta) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}

	// request shape
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if !gotReq.Stream || gotReq.StreamOptions == nil || !gotReq.StreamOptions.IncludeUsage {
		t.Errorf("stream flags = %v %+v", gotReq.Stream, gotReq.StreamOptions)
	}
	if gotReq.MaxTokens != defaultMaxTokens {
		t.Errorf("max tokens = %d", gotReq.MaxTokens)
	}
	wantRoles := []string{"system", "user", "assistant", "tool", "user"}
	if len(gotReq.Messages) != len(wantRoles) {
		t.Fatalf("messages = %d, want %d: %+v", len(gotReq.Messages), len(wantRoles), gotReq.Messages)
	}
	for i, r := range wantRoles {
		if gotReq.Messages[i].Role != r {
			t.Errorf("message %d role = %s, want %s", i, gotReq.Messages[i].Role, r)
		}
	}
	if c := gotReq.Messages[1].Content; c == nil || *c != "hi\nagain" {
		t.Errorf("merged user content = %v", c)
	}
	asst := gotReq.Messages[2]
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "call_0" || asst.ToolCalls[0].Function.Arguments != `{"p":"."}` || asst.ToolCalls[0].Type != "function" {
		t.Errorf("assistant tool_calls = %+v", asst.ToolCalls)
	}
	if tool := gotReq.Messages[3]; tool.ToolCallID != "call_0" || tool.Content == nil || *tool.Content != "a.go" {
		t.Errorf("tool message = %+v", tool)
	}
	if len(gotReq.Tools) != 1 || gotReq.Tools[0].Type != "function" || gotReq.Tools[0].Function.Name != "read_file" {
		t.Errorf("tools = %+v", gotReq.Tools)
	}

	// response
	want := []model.Block{
		{Type: model.BlockThinking, Text: "thinking..."},
		{Type: model.BlockText, Text: "Hello world"},
		{Type: model.BlockToolUse, ID: "call_abc", Name: "read_file", Input: json.RawMessage(`{"path":"a.go"}`)},
	}
	if len(resp.Blocks) != len(want) {
		t.Fatalf("blocks = %+v", resp.Blocks)
	}
	for i := range want {
		g, w := resp.Blocks[i], want[i]
		if g.Type != w.Type || g.Text != w.Text || g.ID != w.ID || g.Name != w.Name || string(g.Input) != string(w.Input) {
			t.Errorf("block %d = %+v, want %+v", i, g, w)
		}
	}
	if resp.StopReason != model.StopToolUse {
		t.Errorf("stop = %s", resp.StopReason)
	}
	if resp.Usage != (model.Usage{InputTokens: 20, OutputTokens: 30, CacheReadTokens: 100}) {
		t.Errorf("usage = %+v", resp.Usage)
	}

	// deltas
	var text, thinking string
	var toolNames []string
	for _, d := range deltas {
		text += d.Text
		thinking += d.Thinking
		if d.ToolName != "" {
			toolNames = append(toolNames, d.ToolName)
		}
	}
	if text != "Hello world" || thinking != "thinking..." || strings.Join(toolNames, ",") != "read_file" {
		t.Errorf("deltas: text=%q thinking=%q tools=%v", text, thinking, toolNames)
	}
}

func TestCompleteHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key","type":"invalid_request_error"}}`)
	}))
	defer srv.Close()
	m, _ := NewWithToken("x", srv.URL, token("t"), Traits{}).Open("m")
	_, err := m.Complete(context.Background(), model.Request{Model: "m"}, nil)
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "bad key") {
		t.Errorf("err = %v", err)
	}
}

func TestCompleteCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	m, _ := NewWithToken("x", srv.URL, token("t"), Traits{}).Open("m")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var resp model.Response
	var err error
	go func() {
		defer close(done)
		resp, err = m.Complete(ctx, model.Request{Model: "m"}, func(d model.Delta) {
			if d.Text == "partial" {
				cancel()
			}
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Complete did not return after cancel")
	}
	if err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if len(resp.Blocks) != 1 || resp.Blocks[0].Text != "partial" {
		t.Errorf("partial blocks = %+v", resp.Blocks)
	}
}

func token(access string) model.TokenSource {
	return func(context.Context) (model.Token, error) { return model.Token{Access: access}, nil }
}

// TestGrokCacheRouting: for xAI a request naming its conversation sends
// x-grok-conv-id, and earlier reasoning is replayed as reasoning_content
// (leaving it out is xAI's top cause of cache misses); another provider
// gets neither, and reasoning with an opaque payload (another API's) is
// never replayed.
func TestGrokCacheRouting(t *testing.T) {
	var gotConv string
	var gotReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotConv = r.Header.Get("x-grok-conv-id")
		body, _ := io.ReadAll(r.Body)
		gotReq = nil
		_ = json.Unmarshal(body, &gotReq)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, cannedStream)
	}))
	defer srv.Close()
	history := []model.Message{
		{Role: model.RoleUser, Blocks: []model.Block{{Type: model.BlockText, Text: "hi"}}},
		{Role: model.RoleAssistant, Blocks: []model.Block{
			{Type: model.BlockThinking, Text: "let me think"},
			{Type: model.BlockThinking, Text: "summary", ProviderID: "rs_1", Opaque: "enc"},
			{Type: model.BlockText, Text: "hello"},
		}},
		{Role: model.RoleUser, Blocks: []model.Block{{Type: model.BlockText, Text: "again"}}},
	}
	assistant := func() map[string]any {
		for _, m := range gotReq["messages"].([]any) {
			if msg := m.(map[string]any); msg["role"] == "assistant" {
				return msg
			}
		}
		return nil
	}
	for _, c := range []struct {
		provider      string
		traits        Traits
		wantConv      string
		wantReasoning any
	}{{"routed by header", Traits{CacheHeader: "x-grok-conv-id", ReplaysReasoning: true}, "agent-1", "let me think"}, {"no traits", Traits{}, "", nil}} {
		m, _ := NewWithToken(c.provider, srv.URL, token("tok"), c.traits).Open("grok-4")
		if _, err := m.Complete(context.Background(), model.Request{Model: "grok-4", Messages: history, CacheKey: "agent-1"}, nil); err != nil {
			t.Fatal(err)
		}
		if gotConv != c.wantConv || assistant()["reasoning_content"] != c.wantReasoning {
			t.Fatalf("%s: conv id %q, reasoning %v", c.provider, gotConv, assistant()["reasoning_content"])
		}
	}
}
