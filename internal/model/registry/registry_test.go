package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
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
 "zai-coding-plan":{"id":"zai-coding-plan","api":"https://api.z.ai/api/coding/paas/v4","models":{
   "glm-5.3":{"id":"glm-5.3","name":"GLM-5.3","limit":{"context":200000,"output":128000},"cost":{"input":0,"output":0}},
   "glm-5.3-flash":{"id":"glm-5.3-flash","limit":{"context":200000}},
   "text-embedding":{"id":"text-embedding","limit":{"context":1}}}},
 "zai":{"id":"zai","api":"https://api.z.ai/api/paas/v4","models":{"glm-4.5":{"id":"glm-4.5","limit":{"context":1}}}},
 "kimi-code-plan-global":{"id":"kimi-code-plan-global","api":"https://api.kimi.ai/coding/v1","models":{
   "kimi-for-coding":{"id":"kimi-for-coding","name":"Kimi For Coding","limit":{"context":1048576},"cost":{"input":0,"output":0}},
   "k3":{"id":"k3","limit":{"context":1048576}}}},
 "moonshotai":{"id":"moonshotai","api":"https://api.moonshot.ai/v1","models":{"kimi-k2.7-code":{"id":"kimi-k2.7-code","limit":{"context":1}}}},
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
	r := New(cat).WithStore(auth.Open(filepath.Join(t.TempDir(), "auth.json"))).WithFlows(map[string]oauth.Flow{"openai": fo, "xai": fx, "zai": oauth.ZAI(), "kimi": oauth.Kimi()})
	return r, fo, fx
}

func TestOnlySubscriptionsAreOffered(t *testing.T) {
	r, _, _ := newReg(t)
	l := r.List()
	if len(l) != 4 || l[0].ID != "openai" || l[0].Name != "ChatGPT" || l[1].ID != "xai" || l[1].Name != "Grok" || l[0].Connected {
		t.Fatalf("%+v", l)
	}
	// zai reads the coding plan's catalog entry, not the pay-as-you-go one,
	// and keeps only its GLM models
	if l[2].ID != "zai" || l[2].Name != "Z.ai Coding Plan" || l[2].Models != 2 || l[2].Connected {
		t.Fatalf("zai: %+v", l[2])
	}
	if l[2].Methods[0].ID != oauth.MethodAPIKey {
		t.Fatalf("zai signs in with a key: %+v", l[2].Methods)
	}
	// kimi likewise reads its plan's entry, and every model in it is the
	// plan's, so it filters nothing
	if l[3].ID != "kimi" || l[3].Name != "Kimi For Coding" || l[3].Models != 2 || l[3].Methods[0].ID != oauth.MethodAPIKey {
		t.Fatalf("kimi: %+v", l[3])
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
	r.WithEndpoints(srv.URL, "", "", "")
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
	r.WithEndpoints("", srv.URL, "", "")
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
	r.WithEndpoints("http://127.0.0.1:1", "", "", "")
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

// TestPlanUsageTracking: the newest observed usage per provider is kept
// and handed to the hook; an older reading (a slower call finishing last,
// or a stale one restored from disk) never replaces a newer one.
func TestPlanUsageTracking(t *testing.T) {
	r := New(nil)
	t0 := time.Unix(1_700_000_000, 0)
	var hooked []model.PlanUsage
	r.OnPlanUsage(func(provider string, u model.PlanUsage) {
		if provider == "openai" {
			hooked = append(hooked, u)
		}
	})
	r.SeedPlanUsage("openai", model.PlanUsage{Observed: t0, Windows: []model.UsageWindow{{UsedPercent: 10}}})
	r.observeUsage("openai", model.PlanUsage{Observed: t0.Add(time.Minute), Windows: []model.UsageWindow{{UsedPercent: 20}}})
	r.observeUsage("openai", model.PlanUsage{Observed: t0.Add(30 * time.Second), Windows: []model.UsageWindow{{UsedPercent: 15}}})
	r.SeedPlanUsage("openai", model.PlanUsage{Observed: t0, Windows: []model.UsageWindow{{UsedPercent: 5}}})
	if got := r.PlanUsage()["openai"]; got.Windows[0].UsedPercent != 20 || len(hooked) != 1 {
		t.Fatalf("kept %+v, hooked %d", got, len(hooked))
	}
	if SubscriptionName("openai") != "ChatGPT" {
		t.Fatal(SubscriptionName("openai"))
	}
}

// TestZaiSignsInWithAKey: the GLM Coding Plan stores the pasted key rather
// than an OAuth pair, sends it as a bearer token, and never refreshes —
// there is no session to rotate.
func TestZaiSignsInWithAKey(t *testing.T) {
	r, _, _ := newReg(t)
	if err := r.SaveLogin("zai", oauth.Tokens{Access: "zk-123"}); err != nil {
		t.Fatal(err)
	}
	c, ok := r.store.Get("zai")
	if !ok || c.Type != auth.TypeAPIKey || c.Key != "zk-123" || c.Access != "" || c.Refresh != "" {
		t.Fatalf("stored credential: %+v", c)
	}
	if st, ok := r.Status("zai"); !ok || !st.Connected {
		t.Fatalf("a stored key connects the provider: %+v", st)
	}

	var gotAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotAuth.Store(req.Header.Get("Authorization"))
		if !strings.HasSuffix(req.URL.Path, "/chat/completions") {
			t.Errorf("path %s", req.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ni\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	r.WithEndpoints("", "", srv.URL, "")
	m, info, err := r.Resolve("zai/glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	if info.ContextWindow != 200000 {
		t.Fatalf("metadata comes from the plan's catalog entry: %+v", info)
	}
	resp, err := m.Complete(context.Background(), model.Request{Model: "glm-5.3", Messages: []model.Message{{Role: model.RoleUser, Blocks: []model.Block{{Type: model.BlockText, Text: "x"}}}}}, nil)
	if err != nil || resp.Blocks[0].Text != "ni" || gotAuth.Load() != "Bearer zk-123" {
		t.Fatalf("%+v %v %v", resp, err, gotAuth.Load())
	}
}

// TestZaiServesThePlanNotTheAPI: the models offered are the coding plan's,
// not the pay-as-you-go catalog that shares the vendor's name — the two
// endpoints look alike and bill differently.
func TestZaiServesThePlanNotTheAPI(t *testing.T) {
	r, _, _ := newReg(t)
	if err := r.SaveLogin("zai", oauth.Tokens{Access: "zk"}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range r.Models("zai", false) {
		ids = append(ids, m.ID)
	}
	want := []string{"zai/glm-5.3", "zai/glm-5.3-flash"}
	if !slices.Equal(ids, want) {
		t.Fatalf("plan models: %v, want %v", ids, want)
	}
	// and the endpoint is the plan's. The two URLs differ by one path
	// segment and bill differently: /api/coding/paas/v4 draws on the
	// subscription, /api/paas/v4 spends pay-as-you-go credits.
	if zaiBaseURL != "https://api.z.ai/api/coding/paas/v4" {
		t.Fatalf("the plan endpoint, not the API: %s", zaiBaseURL)
	}
}

// TestKimiServesThePlanNotTheAPI: Kimi For Coding is the subscription —
// its own endpoint, its own models — not the Moonshot platform that sells
// the same family by the token.
func TestKimiServesThePlanNotTheAPI(t *testing.T) {
	r, _, _ := newReg(t)
	if err := r.SaveLogin("kimi", oauth.Tokens{Access: "kk-1"}); err != nil {
		t.Fatal(err)
	}
	c, ok := r.store.Get("kimi")
	if !ok || c.Type != auth.TypeAPIKey || c.Key != "kk-1" {
		t.Fatalf("stored credential: %+v", c)
	}
	var ids []string
	for _, m := range r.Models("kimi", false) {
		ids = append(ids, m.ID)
	}
	if want := []string{"kimi/k3", "kimi/kimi-for-coding"}; !slices.Equal(ids, want) {
		t.Fatalf("plan models: %v, want %v", ids, want)
	}
	if kimiBaseURL != "https://api.kimi.ai/coding/v1" {
		t.Fatalf("the plan endpoint, not the platform: %s", kimiBaseURL)
	}
}

// TestPlanUsageIsAskedForAfterACall: Z.ai reports plan usage nowhere but its
// usage endpoint, so a model call is followed by one question to it, on the
// host the key already goes to and with the key sent the way that endpoint
// takes it; a second call soon after asks nothing, and a signed-out provider
// is never asked.
func TestPlanUsageIsAskedForAfterACall(t *testing.T) {
	r, _, _ := newReg(t)
	if err := r.SaveLogin("zai", oauth.Tokens{Access: "zk-123"}); err != nil {
		t.Fatal(err)
	}
	var asked atomic.Int32
	var quotaAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api/monitor/usage/quota/limit" {
			asked.Add(1)
			quotaAuth.Store(req.Header.Get("Authorization"))
			w.Write([]byte(`{"code":200,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":6,"percentage":40,"nextResetTime":1800500000000},{"type":"TOKENS_LIMIT","unit":3,"percentage":12,"nextResetTime":1800000000000}]}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ni\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	r.WithEndpoints(srv.URL+"/backend-api/codex/responses", srv.URL+"/v1", srv.URL+"/api/coding/paas/v4", srv.URL+"/coding/v1")
	got := make(chan model.PlanUsage, 4)
	r.OnPlanUsage(func(provider string, u model.PlanUsage) {
		if provider == "zai" {
			got <- u
		}
	})
	m, _, err := r.Resolve("zai/glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	req := model.Request{Model: "glm-5.3", Messages: []model.Message{{Role: model.RoleUser, Blocks: []model.Block{{Type: model.BlockText, Text: "x"}}}}}
	if _, err := m.Complete(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if asked.Load() != 0 {
		t.Fatal("asked before polling was enabled")
	}
	r.EnablePlanPolling()
	for range 2 { // two calls, one question
		if _, err := m.Complete(context.Background(), req, nil); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case u := <-got:
		if len(u.Windows) != 2 || u.Windows[0].Minutes != 300 || u.Windows[0].UsedPercent != 12 || u.Windows[1].Minutes != 10080 || u.Windows[1].UsedPercent != 40 {
			t.Fatalf("reading: %+v", u)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reading after a model call")
	}
	time.Sleep(50 * time.Millisecond)
	if asked.Load() != 1 || quotaAuth.Load() != "zk-123" {
		t.Fatalf("asked %d times with Authorization %q", asked.Load(), quotaAuth.Load())
	}
	// the chart opening asks again; kimi and xai are signed out and are not asked
	r.PollAllPlanUsage(context.Background(), 0)
	if asked.Load() != 2 {
		t.Fatalf("a forced poll asked %d times in all", asked.Load())
	}
	if _, ok := r.PlanUsage()["kimi"]; ok {
		t.Fatal("a signed-out provider has a reading")
	}
}

// TestPlanNameSurvivesAHeaderReading: ChatGPT's usage endpoint names the
// plan and its model calls' headers do not; the name is kept.
func TestPlanNameSurvivesAHeaderReading(t *testing.T) {
	r := New(nil)
	t0 := time.Unix(1_700_000_000, 0)
	r.observeUsage("openai", model.PlanUsage{Plan: "pro", Observed: t0, Windows: []model.UsageWindow{{UsedPercent: 10, Minutes: 300}}})
	r.observeUsage("openai", model.PlanUsage{Observed: t0.Add(time.Minute), Windows: []model.UsageWindow{{UsedPercent: 11, Minutes: 300}}})
	if got := r.PlanUsage()["openai"]; got.Plan != "pro" || got.Windows[0].UsedPercent != 11 {
		t.Fatalf("%+v", got)
	}
}
