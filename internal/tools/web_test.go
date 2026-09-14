package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/policy"
)

const samplePage = `<!doctype html><html><head><title>T</title><style>p{}</style><script>x()</script></head>
<body><nav><a href="/x">skip me</a></nav>
<main>
<h1>Go modules</h1>
<p>Modules are <strong>collections</strong> of <a href="/ref/mod">packages</a>. See <code>go.mod</code>.</p>
<ul><li>one</li><li>two <em>items</em></li></ul>
<ol><li>first</li><li>second</li></ol>
<pre><code class="language-go">func main() {
	fmt.Println("hi")
}</code></pre>
<blockquote><p>quoted words</p></blockquote>
<table><tr><th>a</th><th>b</th></tr><tr><td>1</td><td>2</td></tr></table>
<img src="x.png" alt="a diagram">
</main>
<footer>footer noise</footer></body></html>`

func TestHTMLToMarkdown(t *testing.T) {
	base, _ := url.Parse("https://go.dev/doc/modules")
	md := htmlToMarkdown([]byte(samplePage), base)
	for _, want := range []string{
		"# Go modules",
		"Modules are **collections** of [packages](https://go.dev/ref/mod). See `go.mod`.",
		"- one\n- two *items*",
		"1. first\n2. second",
		"```go\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n```",
		"> quoted words",
		"| a | b |\n| --- | --- |\n| 1 | 2 |",
		"[image: a diagram]",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
	for _, gone := range []string{"skip me", "footer noise", "x()", "p{}"} {
		if strings.Contains(md, gone) {
			t.Errorf("%q should have been dropped:\n%s", gone, md)
		}
	}
	if strings.Contains(md, "\n\n\n") {
		t.Errorf("triple blank lines:\n%q", md)
	}
}

func TestWebFetch(t *testing.T) {
	t.Setenv("STAVLOS_WEB_ALLOW_LOCAL", "1")
	var hits int
	long := strings.Repeat("word ", 9000) // 45k chars: three pages
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch r.URL.Path {
		case "/page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(samplePage))
		case "/long":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(long))
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/away":
			http.Redirect(w, r, "https://example.org/elsewhere", http.StatusFound)
		case "/here":
			http.Redirect(w, r, "/page", http.StatusFound)
		case "/downgrade":
			http.Redirect(w, r, "http://"+r.Host+"/page", http.StatusFound)
		case "/bin":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte{0, 1, 2, 3})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	// the test server's certificate is self-signed: trust it for the fetch client
	old := webTransportHook
	webTransportHook = func(tr *http.Transport) {
		tr.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
	}
	defer func() { webTransportHook = old }()
	webCacheMu.Lock()
	webCache = map[string]webPage{}
	webCacheMu.Unlock()

	ctx := context.Background()
	env := &Env{Dir: t.TempDir()}
	sh := Builtin()["web_fetch"]
	// Policy sees the URL as it will be fetched, whatever its spelling.
	for in, want := range map[string]string{
		` https://go.dev/x `:                                     "https://go.dev/x",
		`http://x.slack.com/y`:                                   "https://x.slack.com/y",
		`HTTPS://X.SLACK.COM/y`:                                  "https://x.slack.com/y",
		`https://user:pw@x.slack.com:443/y#frag`:                 "https://x.slack.com/y",
		`x.slack.com/y`:                                          "https://x.slack.com/y",
		`https://github.com/nicodes/stavlos/blob/main/README.md`: "https://raw.githubusercontent.com/nicodes/stavlos/main/README.md",
		`ftp://x/y`:                                              "ftp://x/y", // unparseable as a fetch: matched as written, then refused
	} {
		if sub := sh.Subject(json.RawMessage(`{"url":"` + in + `"}`)); sub.Kind != policy.KindURL || sub.Primary() != want {
			t.Errorf("Subject(%q) = %+v want %q", in, sub, want)
		}
	}
	r := sh.Run(ctx, json.RawMessage(`{"url":"`+srv.URL+`/page"}`), env)
	if r.IsError || !strings.HasPrefix(r.Output, "[web_fetch: "+srv.URL+"/page · html→markdown · ") || !strings.Contains(r.Output, "Untrusted content") || !strings.Contains(r.Output, "# Go modules") {
		t.Fatalf("page: %+v", r)
	}
	// cached: a second call does not hit the server
	before := hits
	if r = sh.Run(ctx, json.RawMessage(`{"url":"`+srv.URL+`/page"}`), env); r.IsError || hits != before {
		t.Fatalf("cache: hits %d → %d %+v", before, hits, r)
	}
	// paging
	r = sh.Run(ctx, json.RawMessage(`{"url":"`+srv.URL+`/long"}`), env)
	if r.IsError || !strings.Contains(r.Output, "showing 0–20000") || !strings.Contains(r.Output, "start=20000 for more") || len(r.Output) > 20_400 {
		t.Fatalf("page 1: %d chars, head %q", len(r.Output), r.Output[:min(200, len(r.Output))])
	}
	r = sh.Run(ctx, json.RawMessage(`{"url":"`+srv.URL+`/long","start":40000}`), env)
	if r.IsError || !strings.Contains(r.Output, "showing 40000–44999") || strings.Contains(r.Output, "for more") {
		t.Fatalf("last page: %q", r.Output[:min(200, len(r.Output))])
	}
	if r = sh.Run(ctx, json.RawMessage(`{"url":"`+srv.URL+`/long","start":99999}`), env); !r.IsError {
		t.Fatal("start past the end should be an error")
	}
	// other text types come back as they are
	if r = sh.Run(ctx, json.RawMessage(`{"url":"`+srv.URL+`/json"}`), env); r.IsError || !strings.Contains(r.Output, "· application/json ·") || !strings.HasSuffix(r.Output, `{"ok":true}`) {
		t.Fatalf("json: %+v", r)
	}
	// redirects: same host followed, another host reported
	if r = sh.Run(ctx, json.RawMessage(`{"url":"`+srv.URL+`/here"}`), env); r.IsError || !strings.Contains(r.Output, "# Go modules") {
		t.Fatalf("same-host redirect: %+v", r)
	}
	if r = sh.Run(ctx, json.RawMessage(`{"url":"`+srv.URL+`/away"}`), env); !r.IsError || !strings.Contains(r.Output, "redirects to another host: https://example.org/elsewhere") {
		t.Fatalf("cross-host redirect: %+v", r)
	}
	if r = sh.Run(ctx, json.RawMessage(`{"url":"`+srv.URL+`/downgrade"}`), env); !r.IsError || !strings.Contains(r.Output, "plain http") {
		t.Fatalf("https→http redirect: %+v", r)
	}
	// binary, 404, bad scheme
	if r = sh.Run(ctx, json.RawMessage(`{"url":"`+srv.URL+`/bin"}`), env); !r.IsError || !strings.Contains(r.Output, "binary") {
		t.Fatalf("binary: %+v", r)
	}
	if r = sh.Run(ctx, json.RawMessage(`{"url":"`+srv.URL+`/nope"}`), env); !r.IsError || !strings.Contains(r.Output, "HTTP 404") {
		t.Fatalf("404: %+v", r)
	}
	if r = sh.Run(ctx, json.RawMessage(`{"url":"ftp://x/y"}`), env); !r.IsError || !strings.Contains(r.Output, "unsupported scheme") {
		t.Fatalf("scheme: %+v", r)
	}
	// without the override, local addresses are refused before any request
	t.Setenv("STAVLOS_WEB_ALLOW_LOCAL", "")
	webCacheMu.Lock()
	webCache = map[string]webPage{}
	webCacheMu.Unlock()
	before = hits
	if r = sh.Run(ctx, json.RawMessage(`{"url":"`+srv.URL+`/page"}`), env); !r.IsError || !strings.Contains(r.Output, "private or local address") || hits != before {
		t.Fatalf("local address: %+v hits %d→%d", r, before, hits)
	}
	// http is upgraded, credentials dropped; a GitHub blob becomes the raw file
	u, err := parseWebURL("http://user:pw@Example.com:443/a?b=c#frag")
	if err != nil || u.String() != "https://example.com/a?b=c" {
		t.Fatalf("normalise: %v %v", u, err)
	}
	if u, _ = parseWebURL("https://Example.com:8443/a"); u.String() != "https://example.com:8443/a" {
		t.Fatalf("non-default port kept: %v", u)
	}
	if u, _ = parseWebURL("https://github.com/nicodes/stavlos/blob/main/README.md"); u.String() != "https://raw.githubusercontent.com/nicodes/stavlos/main/README.md" {
		t.Fatalf("blob → raw: %v", u)
	}
	if u, _ = parseWebURL("https://github.com/nicodes/stavlos/issues/1"); u.String() != "https://github.com/nicodes/stavlos/issues/1" {
		t.Fatalf("non-blob github url should stay: %v", u)
	}
}

func TestPublicIP(t *testing.T) {
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700::1111", "2a00:1450:4001:80b::200e"} {
		if !publicIP(netip.MustParseAddr(s)) {
			t.Errorf("%s should be public", s)
		}
	}
	for _, s := range []string{
		"127.0.0.1", "127.8.8.8", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1", "169.254.169.254", "0.0.0.0", "0.1.2.3",
		"100.64.0.1", "100.127.255.254", "192.0.0.1", "198.18.0.1", "198.19.255.255", "224.0.0.1", "240.0.0.1", "255.255.255.255",
		"::1", "::", "fe80::1", "fc00::1", "fd12::1", "ff02::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254",
		"64:ff9b::a00:1", "100::1", "2001:db8::1",
	} {
		if publicIP(netip.MustParseAddr(s)) {
			t.Errorf("%s should not be public", s)
		}
	}
}

func TestWebSearch(t *testing.T) {
	t.Setenv("STAVLOS_WEB_ALLOW_LOCAL", "1") // the address check applies to search too
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("X-Subscription-Token") + r.Header.Get("Authorization") + r.Header.Get("x-api-key")
		var b [1 << 12]byte
		n, _ := r.Body.Read(b[:])
		gotBody = string(b[:n])
		switch r.URL.Path {
		case "/brave":
			_, _ = w.Write([]byte(`{"web":{"results":[{"title":"Go","url":"https://go.dev","description":"The Go language"},{"title":"Two","url":"https://two","description":"` + strings.Repeat("s", 400) + `"}]}}`))
		case "/tavily":
			_, _ = w.Write([]byte(`{"results":[{"title":"T","url":"https://t","content":"tavily says"}]}`))
		case "/exa":
			_, _ = w.Write([]byte(`{"results":[{"title":"E","url":"https://e","text":"exa text"}]}`))
		case "/fail":
			http.Error(w, `{"error":"bad key"}`, http.StatusUnauthorized)
		case "/redir":
			http.Redirect(w, r, "/brave", http.StatusFound)
		case "/mcp":
			w.Header().Set("Content-Type", "text/event-stream")
			text := "Title: charmbracelet/bubbletea\nURL: https://github.com/charmbracelet/bubbletea/\nPublished: 2020-01-10\nAuthor: charmbracelet\nHighlights:\nGitHub - bubbletea\n...\n# Bubble Tea\nThe fun, functional way to build terminal apps\n\n---\n\nTitle: Second\nURL: https://example.com/2\nHighlights:\nsecond text"
			msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}})
			_, _ = w.Write([]byte("event: message\ndata: " + string(msg) + "\n\n"))
		}
	}))
	defer srv.Close()
	old := searchEndpoints
	searchEndpoints = map[string]string{"brave": srv.URL + "/brave", "tavily": srv.URL + "/tavily", "exa": srv.URL + "/exa", exaMCP: srv.URL + "/mcp"}
	defer func() { searchEndpoints = old }()

	ctx := context.Background()
	ws := Builtin()["web_search"]
	if sub := ws.Subject(json.RawMessage(`{"query":" go modules "}`)); sub.Kind != policy.KindText || sub.Primary() != "go modules" {
		t.Fatalf("subject should be the query: %+v", sub)
	}
	// unconfigured: Exa's keyless MCP endpoint, a JSON-RPC tools/call answered as SSE
	r0 := ws.Run(ctx, json.RawMessage(`{"query":"bubbletea"}`), &Env{})
	if r0.IsError || gotAuth != "" || !strings.Contains(gotBody, `"name":"web_search_exa"`) || !strings.Contains(gotBody, `"numResults":5`) || !strings.Contains(r0.Output, "via exa-mcp (free, no key): 2 results") || !strings.Contains(r0.Output, "1. charmbracelet/bubbletea\n   https://github.com/charmbracelet/bubbletea/\n   GitHub - bubbletea # Bubble Tea The fun, functional way to build terminal apps") || !strings.Contains(r0.Output, "2. Second\n   https://example.com/2\n   second text") {
		t.Fatalf("keyless fallback: %+v body=%s", r0, gotBody)
	}
	if res, err := parseExaMCP([]byte(`{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"rate limited"}]}}`)); err == nil || !strings.Contains(err.Error(), "rate limited") || res != nil {
		t.Fatalf("mcp error: %v %v", res, err)
	}
	r := ws.Run(ctx, json.RawMessage(`{"query":"go modules","n":2}`), &Env{Search: SearchConfig{Provider: "brave", APIKey: "k1"}})
	if r.IsError || gotAuth != "k1" || !strings.Contains(r.Output, "via brave: 2 results") || !strings.Contains(r.Output, "1. Go\n   https://go.dev\n   The Go language") || !strings.Contains(r.Output, "…") {
		t.Fatalf("brave: %+v auth=%q", r, gotAuth)
	}
	r = ws.Run(ctx, json.RawMessage(`{"query":"go modules"}`), &Env{Search: SearchConfig{Provider: "tavily", APIKey: "k2"}})
	if r.IsError || gotAuth != "Bearer k2" || !strings.Contains(gotBody, `"max_results":5`) || !strings.Contains(r.Output, "tavily says") {
		t.Fatalf("tavily: %+v auth=%q body=%s", r, gotAuth, gotBody)
	}
	r = ws.Run(ctx, json.RawMessage(`{"query":"go modules","n":50}`), &Env{Search: SearchConfig{Provider: "exa", APIKey: "k3"}})
	if r.IsError || gotAuth != "k3" || !strings.Contains(gotBody, `"numResults":10`) || !strings.Contains(r.Output, "exa text") {
		t.Fatalf("exa: %+v auth=%q body=%s", r, gotAuth, gotBody)
	}
	searchEndpoints["brave"] = srv.URL + "/fail"
	if r = ws.Run(ctx, json.RawMessage(`{"query":"go"}`), &Env{Search: SearchConfig{Provider: "brave", APIKey: "bad"}}); !r.IsError || !strings.Contains(r.Output, "HTTP 401") || !strings.Contains(r.Output, "bad key") {
		t.Fatalf("failure: %+v", r)
	}
	if r = ws.Run(ctx, json.RawMessage(`{"query":"go"}`), &Env{Search: SearchConfig{Provider: "bing", APIKey: "x"}}); !r.IsError || !strings.Contains(r.Output, "unknown search provider") {
		t.Fatalf("unknown provider: %+v", r)
	}
	// A search backend that redirects is refused (the key would travel with it).
	searchEndpoints["brave"] = srv.URL + "/redir"
	if r = ws.Run(ctx, json.RawMessage(`{"query":"go"}`), &Env{Search: SearchConfig{Provider: "brave", APIKey: "k"}}); !r.IsError || !strings.Contains(r.Output, "redirect") {
		t.Fatalf("redirect: %+v", r)
	}
	// Without the local override the address check stops the call.
	t.Setenv("STAVLOS_WEB_ALLOW_LOCAL", "")
	searchEndpoints["brave"] = srv.URL + "/brave"
	if r = ws.Run(ctx, json.RawMessage(`{"query":"go"}`), &Env{Search: SearchConfig{Provider: "brave", APIKey: "k"}}); !r.IsError || !strings.Contains(r.Output, "private or local address") {
		t.Fatalf("local backend: %+v", r)
	}
}

// TestHTMLToMarkdownScales: the converter appends and trims in place, so a
// page of tens of thousands of blocks converts in linear time, and it stops
// reading once the page cap is passed.
func TestHTMLToMarkdownScales(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("<html><body><main>")
	for i := 0; i < 40000; i++ {
		sb.WriteString("<div><p>para ")
		sb.WriteString(strconv.Itoa(i))
		sb.WriteString(" text   </p><ul><li>a</li><li>b</li></ul></div>")
	}
	sb.WriteString("</main></body></html>")
	start := time.Now()
	md := htmlToMarkdown([]byte(sb.String()), nil)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("conversion took %v", d)
	}
	if len(md) > mdOverflow || !strings.Contains(md, "para 0 text\n\n- a\n- b") {
		t.Fatalf("len %d head %q", len(md), md[:min(80, len(md))])
	}
}
