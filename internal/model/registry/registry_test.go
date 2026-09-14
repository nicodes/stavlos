package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/auth"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/modelsdev"
	"github.com/nicodes/stavlos/internal/oauth"
)

const fixture = `{
 "openai":{"id":"openai","env":["OPENAI_API_KEY"],"npm":"@ai-sdk/openai","models":{
   "gpt-5.4":{"id":"gpt-5.4","name":"GPT-5.4","limit":{"context":400000,"output":128000},"cost":{"input":2,"output":8}},
   "gpt-5.5-pro":{"id":"gpt-5.5-pro","limit":{"context":1000},"cost":{"input":1,"output":1}},
   "gpt-4.1":{"id":"gpt-4.1","limit":{"context":1000},"cost":{"input":1,"output":1}},
   "gpt-5.7":{"id":"gpt-5.7","limit":{"context":1000},"cost":{"input":1,"output":1}}}},
 "xai":{"id":"xai","env":["XAI_API_KEY"],"npm":"@ai-sdk/xai","api":"https://api.x.ai/v1","models":{
   "grok-4":{"id":"grok-4","name":"Grok 4","limit":{"context":256000},"cost":{"input":3,"output":15}},
   "grok-imagine-image":{"id":"grok-imagine-image"},
   "other":{"id":"other","limit":{"context":1}}}},
 "anthropic":{"id":"anthropic","env":["ANTHROPIC_API_KEY"],"npm":"@ai-sdk/anthropic","models":{"claude":{"id":"claude"}}}
}`

type fakeFlow struct {
	provider  string
	refreshed atomic.Int32
}

func (f *fakeFlow) Provider() string { return f.provider }
func (f *fakeFlow) Label() string    { return "fake" }
func (f *fakeFlow) Methods() []oauth.Method {
	return []oauth.Method{{ID: oauth.MethodDevice, Label: "fake device"}}
}
func (f *fakeFlow) Start(context.Context, string) (*oauth.Pending, error) {
	return &oauth.Pending{Provider: f.provider, URL: "https://x/dev", Code: "AB-CD"}, nil
}
func (f *fakeFlow) Wait(context.Context, *oauth.Pending) (oauth.Tokens, error) {
	return oauth.Tokens{Access: "acc1", Refresh: "ref1", ExpiresAt: time.Now().Add(time.Hour), Email: "me@x.y", AccountID: "acct_1"}, nil
}
func (f *fakeFlow) Refresh(_ context.Context, r string) (oauth.Tokens, error) {
	f.refreshed.Add(1)
	return oauth.Tokens{Access: "acc2", Refresh: "ref2", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func newReg(t *testing.T) (*Registry, *fakeFlow, *fakeFlow) {
	cat, err := modelsdev.Parse([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	fo, fx := &fakeFlow{provider: "openai"}, &fakeFlow{provider: "xai"}
	r := New(cat).WithStore(auth.Open(filepath.Join(t.TempDir(), "auth.json"))).WithFlows(map[string]oauth.Flow{"openai": fo, "xai": fx})
	return r, fo, fx
}

func TestOnlyTwoProviders(t *testing.T) {
	r, _, _ := newReg(t)
	l := r.List()
	if len(l) != 2 || l[0].ID != "openai" || l[0].Name != "ChatGPT" || l[1].ID != "xai" || l[1].Name != "Grok" || l[0].Connected {
		t.Fatalf("%+v", l)
	}
	if _, ok := r.Status("anthropic"); ok {
		t.Fatal("anthropic should not be offered")
	}
	if err := r.Check("anthropic/claude"); err == nil || !strings.Contains(err.Error(), "ChatGPT") {
		t.Fatalf("%v", err)
	}
	if err := r.Check("openai/gpt-5.4"); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("%v", err)
	}
	if ms := r.Models("", false); len(ms) != 0 {
		t.Fatalf("models before login: %+v", ms)
	}
	// allow-list applies even when listing all
	ids := map[string]bool{}
	for _, m := range r.Models("", true) {
		ids[m.ID] = true
	}
	if !ids["openai/gpt-5.4"] || !ids["openai/gpt-5.7"] || ids["openai/gpt-5.5-pro"] || ids["openai/gpt-4.1"] || !ids["xai/grok-4"] || ids["xai/other"] || ids["xai/grok-imagine-image"] {
		t.Fatalf("%v", ids)
	}
}

func TestLoginRefreshAndResolve(t *testing.T) {
	r, fo, _ := newReg(t)
	f, err := r.Flow("openai")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := f.Start(context.Background(), "")
	tok, _ := f.Wait(context.Background(), p)
	if err := r.SaveLogin("openai", tok); err != nil {
		t.Fatal(err)
	}
	st, _ := r.Status("openai")
	if !st.Connected || st.Account != "me@x.y" || st.Label != "fake" {
		t.Fatalf("%+v", st)
	}
	if got := r.Providers(); len(got) != 1 || got[0] != "openai" {
		t.Fatalf("%v", got)
	}
	ms := r.Models("", false)
	if len(ms) != 2 || ms[0].Info.InputPrice != 0 || ms[0].Info.ContextWindow != 400000 {
		t.Fatalf("%+v", ms)
	}

	// token source: fresh token → no refresh; near expiry → refresh + persist
	src := r.tokenSource("openai")
	tk, err := src(context.Background())
	if err != nil || tk.Access != "acc1" || tk.AccountID != "acct_1" || fo.refreshed.Load() != 0 {
		t.Fatalf("%+v %v %d", tk, err, fo.refreshed.Load())
	}
	c, _ := r.store.Get("openai")
	c.Expires = time.Now().Add(30 * time.Second).UnixMilli()
	_ = r.store.Set("openai", c)
	tk, err = src(context.Background())
	if err != nil || tk.Access != "acc2" || fo.refreshed.Load() != 1 {
		t.Fatalf("%+v %v %d", tk, err, fo.refreshed.Load())
	}
	if c2, _ := r.store.Get("openai"); c2.Refresh != "ref2" || c2.AccountID != "acct_1" || c2.Email != "me@x.y" {
		t.Fatalf("persisted %+v", c2)
	}

	// resolve goes to the codex endpoint with the bearer + account header
	var gotAuth, gotAcct atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotAuth.Store(req.Header.Get("Authorization"))
		gotAcct.Store(req.Header.Get("ChatGPT-Account-Id"))
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n"))
	}))
	defer srv.Close()
	r.WithEndpoints(srv.URL, "")
	m, info, err := r.Resolve("openai/gpt-5.4")
	if err != nil || info.InputPrice != 0 {
		t.Fatalf("%v %+v", err, info)
	}
	resp, err := m.Complete(context.Background(), model.Request{Model: "gpt-5.4", Messages: []model.Message{{Role: model.RoleUser, Blocks: []model.Block{{Type: model.BlockText, Text: "x"}}}}}, nil)
	if err != nil || len(resp.Blocks) == 0 || resp.Blocks[0].Text != "hi" {
		t.Fatalf("%+v %v", resp, err)
	}
	if gotAuth.Load() != "Bearer acc2" || gotAcct.Load() != "acct_1" {
		t.Fatalf("headers %v %v", gotAuth.Load(), gotAcct.Load())
	}

	if err := r.Disconnect("openai"); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.Status("openai"); st.Connected {
		t.Fatal("still connected")
	}
}

func TestGrokUsesBearer(t *testing.T) {
	r, _, fx := newReg(t)
	_ = fx
	if err := r.SaveLogin("xai", oauth.Tokens{Access: "xa", Refresh: "xr", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	var gotAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotAuth.Store(req.Header.Get("Authorization"))
		if !strings.HasSuffix(req.URL.Path, "/chat/completions") {
			t.Errorf("path %s", req.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"yo\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	r.WithEndpoints("", srv.URL)
	m, _, err := r.Resolve("xai/grok-4")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := m.Complete(context.Background(), model.Request{Model: "grok-4", Messages: []model.Message{{Role: model.RoleUser, Blocks: []model.Block{{Type: model.BlockText, Text: "x"}}}}}, nil)
	if err != nil || resp.Blocks[0].Text != "yo" || gotAuth.Load() != "Bearer xa" {
		t.Fatalf("%+v %v %v", resp, err, gotAuth.Load())
	}
}

// TestProvidersBuiltOnce: a subscription's adapter is built on first use
// and reused; changing the endpoints starts over.
func TestProvidersBuiltOnce(t *testing.T) {
	r, _, _ := newReg(t)
	if err := r.SaveLogin("openai", oauth.Tokens{Access: "a", Refresh: "r", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	p1, _, err := r.lookup("openai/gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}
	p2, _, _ := r.lookup("openai/gpt-5.7")
	if p1 != p2 {
		t.Fatal("the adapter was rebuilt")
	}
	r.WithEndpoints("http://127.0.0.1:1", "")
	if p3, _, _ := r.lookup("openai/gpt-5.4"); p3 == p1 {
		t.Fatal("new endpoints should build a new adapter")
	}
	if err := r.Disconnect("openai"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.lookup("openai/gpt-5.4"); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("after disconnect: %v", err)
	}
	// A catalog swap is seen by the next listing.
	cat, _ := modelsdev.Parse([]byte(`{"xai":{"models":{"grok-9":{"id":"grok-9"}}}}`))
	r.SetCatalog(cat)
	if ms := r.Models("xai", true); len(ms) != 1 || ms[0].ID != "xai/grok-9" {
		t.Fatalf("after SetCatalog: %+v", ms)
	}
	if st, _ := r.Status("xai"); st.Models != 1 {
		t.Fatalf("model count not recomputed for the new catalog: %d", st.Models)
	}
}
