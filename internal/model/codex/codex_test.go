package codex

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
	"github.com/nicodes/stavlos/internal/model/stream"
)

const cannedStream = `data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}

data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

data: {"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"Let me think"}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"Let me think"}],"encrypted_content":"enc1"}}

data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

data: {"type":"response.output_text.delta","output_index":1,"delta":"Hello"}

: keepalive

data: {"type":"response.output_text.delta","output_index":1,"delta":" world"}

data: {"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Hello world"}]}}

data: {"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"bash","arguments":""}}

data: {"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"comm"}

data: {"type":"response.function_call_arguments.delta","output_index":2,"delta":"and\":\"ls\"}"}

data: {"type":"response.function_call_arguments.done","output_index":2,"arguments":"{\"command\":\"ls\"}"}

data: {"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"bash","arguments":"{\"command\":\"ls\"}"}}

data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":120,"output_tokens":30,"input_tokens_details":{"cached_tokens":20}}}}

`

// The 429 test retries; keep its backoff negligible.
func init() { stream.RetryDelay = time.Millisecond }

func staticSource(access, account string) model.TokenSource {
	return func(context.Context) (model.Token, error) {
		return model.Token{Access: access, AccountID: account}, nil
	}
}

func TestResponseHeaderTimeout(t *testing.T) {
	p := NewWithEndpoint(staticSource("tok", "acct"), DefaultEndpoint).(*provider)
	transport, ok := p.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", p.http.Transport)
	}
	if transport.ResponseHeaderTimeout != 3*time.Minute {
		t.Fatalf("response header timeout = %s, want 3m", transport.ResponseHeaderTimeout)
	}
}

func TestCompleteStream(t *testing.T) {
	var gotHdr http.Header
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHdr = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, cannedStream)
	}))
	defer srv.Close()

	p := NewWithEndpoint(staticSource("tok-abc", "acct-1"), srv.URL+"/responses")
	if p.Name() != "openai" {
		t.Errorf("Name = %q", p.Name())
	}
	m, err := p.Open("gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}

	req := model.Request{
		Model:     "gpt-5.4",
		System:    "be brief",
		MaxTokens: 4000, // never forwarded: the backend rejects max_output_tokens
		Tools: []model.ToolDef{{
			Name:        "bash",
			Description: "run a command",
			Schema:      json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`),
		}},
		Messages: []model.Message{
			{Role: model.RoleUser, Blocks: []model.Block{
				{Type: model.BlockText, Text: "list "},
				{Type: model.BlockText, Text: "files"},
			}},
			{Role: model.RoleAssistant, Blocks: []model.Block{
				{Type: model.BlockThinking, ID: "rs_0", Text: "hmm", Signature: "enc0"},
				{Type: model.BlockThinking, Text: "unsigned, must be skipped"},
				{Type: model.BlockToolUse, ID: "call_0", Name: "bash", Input: json.RawMessage(`{"command":"pwd"}`)},
			}},
			{Role: model.RoleUser, Blocks: []model.Block{
				{Type: model.BlockToolResult, ToolUseID: "call_0", Content: "/home", IsError: false},
			}},
		},
	}

	var deltas []model.Delta
	resp, err := m.Complete(context.Background(), req, func(d model.Delta) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if _, ok := gotBody["max_output_tokens"]; ok {
		t.Error("max_output_tokens must not be sent: the ChatGPT backend rejects it")
	}

	// Headers.
	if got := gotHdr.Get("Authorization"); got != "Bearer tok-abc" {
		t.Errorf("Authorization = %q", got)
	}
	if got := gotHdr.Get("ChatGPT-Account-Id"); got != "acct-1" {
		t.Errorf("ChatGPT-Account-Id = %q", got)
	}
	if got := gotHdr.Get("originator"); got != "stavlos" {
		t.Errorf("originator = %q", got)
	}
	if got := gotHdr.Get("User-Agent"); !strings.HasPrefix(got, "stavlos/0.1 (") {
		t.Errorf("User-Agent = %q", got)
	}
	if got := gotHdr.Get("Accept"); got != "text/event-stream" {
		t.Errorf("Accept = %q", got)
	}

	// Body shape.
	if gotBody["model"] != "gpt-5.4" {
		t.Errorf("model = %v", gotBody["model"])
	}
	if gotBody["store"] != false || gotBody["stream"] != true {
		t.Errorf("store/stream = %v/%v", gotBody["store"], gotBody["stream"])
	}
	if gotBody["instructions"] != "be brief" {
		t.Errorf("instructions = %v", gotBody["instructions"])
	}
	if gotBody["tool_choice"] != "auto" || gotBody["parallel_tool_calls"] != true {
		t.Errorf("tool_choice/parallel = %v/%v", gotBody["tool_choice"], gotBody["parallel_tool_calls"])
	}
	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", gotBody["tools"])
	}
	tool0 := tools[0].(map[string]any)
	if tool0["type"] != "function" || tool0["name"] != "bash" || tool0["strict"] != false {
		t.Errorf("tools[0] = %v", tool0)
	}
	if _, ok := tool0["parameters"].(map[string]any); !ok {
		t.Errorf("tools[0].parameters = %v", tool0["parameters"])
	}

	input, _ := gotBody["input"].([]any)
	if len(input) != 4 {
		t.Fatalf("input has %d items, want 4: %v", len(input), input)
	}
	it0 := input[0].(map[string]any)
	if it0["type"] != "message" || it0["role"] != "user" {
		t.Errorf("input[0] = %v", it0)
	}
	if c, _ := it0["content"].([]any); len(c) != 2 ||
		c[0].(map[string]any)["type"] != "input_text" ||
		c[0].(map[string]any)["text"] != "list " ||
		c[1].(map[string]any)["text"] != "files" {
		t.Errorf("input[0].content = %v", it0["content"])
	}
	it1 := input[1].(map[string]any)
	if it1["type"] != "reasoning" || it1["id"] != "rs_0" || it1["encrypted_content"] != "enc0" {
		t.Errorf("input[1] = %v", it1)
	}
	if s, ok := it1["summary"].([]any); !ok || len(s) != 0 {
		t.Errorf("input[1].summary = %v (want empty array)", it1["summary"])
	}
	it2 := input[2].(map[string]any)
	if it2["type"] != "function_call" || it2["call_id"] != "call_0" || it2["name"] != "bash" ||
		it2["arguments"] != `{"command":"pwd"}` {
		t.Errorf("input[2] = %v", it2)
	}
	it3 := input[3].(map[string]any)
	if it3["type"] != "function_call_output" || it3["call_id"] != "call_0" || it3["output"] != "/home" {
		t.Errorf("input[3] = %v", it3)
	}

	// Response.
	want := []model.Block{
		{Type: model.BlockThinking, ID: "rs_1", Text: "Let me think", Signature: "enc1"},
		{Type: model.BlockText, Text: "Hello world"},
		{Type: model.BlockToolUse, ID: "call_1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)},
	}
	if len(resp.Blocks) != len(want) {
		t.Fatalf("blocks = %+v", resp.Blocks)
	}
	for i := range want {
		g, w := resp.Blocks[i], want[i]
		if g.Type != w.Type || g.ID != w.ID || g.Text != w.Text || g.Signature != w.Signature ||
			g.Name != w.Name || string(g.Input) != string(w.Input) {
			t.Errorf("block[%d] = %+v, want %+v", i, g, w)
		}
	}
	if resp.StopReason != model.StopToolUse {
		t.Errorf("stop = %q", resp.StopReason)
	}
	if resp.Usage != (model.Usage{InputTokens: 100, OutputTokens: 30, CacheReadTokens: 20}) {
		t.Errorf("usage = %+v", resp.Usage)
	}

	// Deltas: thinking, two text, one tool name.
	var text, thinking string
	var names []string
	for _, d := range deltas {
		text += d.Text
		thinking += d.Thinking
		if d.ToolName != "" {
			names = append(names, d.ToolName)
		}
	}
	if text != "Hello world" || thinking != "Let me think" || len(names) != 1 || names[0] != "bash" {
		t.Errorf("deltas: text=%q thinking=%q names=%v", text, thinking, names)
	}
}

func TestCompleteUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad token"}}`)
	}))
	defer srv.Close()

	m, _ := NewWithEndpoint(staticSource("expired", ""), srv.URL).Open("gpt-5.4")
	_, err := m.Complete(context.Background(), model.Request{Model: "gpt-5.4"}, nil)
	if err == nil || !strings.Contains(err.Error(), "unauthorized") || !strings.Contains(err.Error(), "/providers") {
		t.Errorf("err = %v", err)
	}
}

func TestCompleteOtherError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit","message":"slow down"}}`)
	}))
	defer srv.Close()

	m, _ := NewWithEndpoint(staticSource("tok", ""), srv.URL).Open("gpt-5.4")
	_, err := m.Complete(context.Background(), model.Request{Model: "gpt-5.4"}, nil)
	if err == nil || !strings.Contains(err.Error(), "codex: status 429") || !strings.Contains(err.Error(), "slow down") {
		t.Errorf("err = %v", err)
	}
}

func TestCompleteCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1"}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","output_index":0,"delta":"partial"}`+"\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	m, _ := NewWithEndpoint(staticSource("tok", ""), srv.URL).Open("gpt-5.4")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var resp model.Response
	var err error
	go func() {
		defer close(done)
		resp, err = m.Complete(ctx, model.Request{Model: "gpt-5.4"}, func(d model.Delta) {
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
	if len(resp.Blocks) != 1 || resp.Blocks[0].Type != model.BlockText || resp.Blocks[0].Text != "partial" {
		t.Errorf("partial blocks = %+v", resp.Blocks)
	}
}
