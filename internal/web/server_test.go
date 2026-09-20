package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gorilla/websocket"
)

// echoLines answers every protocol line with the same line, in two writes,
// so the test also covers a line split across writes.
func echoLines(_ context.Context, nc net.Conn) {
	sc := bufio.NewScanner(nc)
	for sc.Scan() {
		line := sc.Text()
		_, _ = nc.Write([]byte(line[:len(line)/2]))
		_, _ = nc.Write([]byte(line[len(line)/2:] + "\n"))
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func start(t *testing.T, hosts ...string) (*Server, string) {
	t.Helper()
	s := New(Options{Port: freePort(t), Hosts: hosts, Serve: echoLines, Sheet: testSheet,
		Assets: fstest.MapFS{"index.html": {Data: []byte("<!doctype html>app")}, "assets/app.js": {Data: []byte("1")}, "sheet.css": {Data: []byte(".btn{}")}}})
	if err := s.Enable(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Disable)
	return s, strings.TrimSuffix(s.URL(), "/")
}

func do(t *testing.T, method, target string, hdr map[string]string, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

// pageKeys is what each signed-in test browser keeps in its localStorage.
var pageKeys sync.Map // cookie value → page key

// socketHeader is what a signed-in page sends to open its socket.
func socketHeader(origin string, c *http.Cookie) http.Header {
	h := http.Header{"Origin": {origin}}
	if c != nil {
		h.Set("Cookie", c.Name+"="+c.Value)
		if key, ok := pageKeys.Load(c.Value); ok {
			h.Set("Sec-WebSocket-Protocol", "stavlos, key."+key.(string))
		}
	}
	return h
}

func signIn(t *testing.T, s *Server, base string) *http.Cookie {
	t.Helper()
	open, err := s.OpenURL()
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(open)
	code := strings.TrimPrefix(u.Fragment, "code=")
	res := do(t, "POST", base+"/api/session", map[string]string{"Origin": base}, `{"code":"`+code+`"}`)
	var body struct{ Key string }
	if res.StatusCode != http.StatusOK || len(res.Cookies()) != 1 || json.NewDecoder(res.Body).Decode(&body) != nil || body.Key == "" {
		t.Fatalf("sign-in: %d %v key %q", res.StatusCode, res.Cookies(), body.Key)
	}
	pageKeys.Store(res.Cookies()[0].Value, body.Key)
	// the code is spent
	if again := do(t, "POST", base+"/api/session", map[string]string{"Origin": base}, `{"code":"`+code+`"}`); again.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a code worked twice: %d", again.StatusCode)
	}
	return res.Cookies()[0]
}

func TestHostAndOriginGuards(t *testing.T) {
	s, base := start(t, "box.tailnet.ts.net")
	if res := do(t, "GET", base+"/", map[string]string{"Host": "evil.example:80"}, ""); res.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("a rebound host was served: %d", res.StatusCode)
	}
	if res := do(t, "GET", base+"/", map[string]string{"Host": "box.tailnet.ts.net"}, ""); res.StatusCode != http.StatusOK {
		t.Fatalf("a configured host was refused: %d", res.StatusCode)
	}
	open, _ := s.OpenURL()
	code := open[strings.Index(open, "#code=")+6:]
	for _, origin := range []string{"", "null", "https://evil.example", "http://127.0.0.1", "http://127.0.0.1:1"} {
		if res := do(t, "POST", base+"/api/session", map[string]string{"Origin": origin}, `{"code":"`+code+`"}`); res.StatusCode != http.StatusForbidden {
			t.Fatalf("origin %q: %d", origin, res.StatusCode)
		}
	}
	// a proxy's origin is ours, and its cookie is Secure
	res := do(t, "POST", base+"/api/session", map[string]string{"Origin": "https://box.tailnet.ts.net"}, `{"code":"`+code+`"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the configured origin was refused: %d", res.StatusCode)
	}
	if c := res.Cookies()[0]; !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie flags: %+v", c)
	}
}

func TestPageHeaders(t *testing.T) {
	_, base := start(t)
	res := do(t, "GET", base+"/some/route", nil, "")
	csp := res.Header.Get("Content-Security-Policy")
	if res.StatusCode != 200 || !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") || res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("%d %q", res.StatusCode, csp)
	}
	if res := do(t, "GET", base+"/assets/missing.js", nil, ""); res.StatusCode != http.StatusNotFound {
		t.Fatalf("a missing asset got the app: %d", res.StatusCode)
	}
}

func TestWebSocketNeedsSessionAndCarriesLines(t *testing.T) {
	s, base := start(t)
	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/ws"
	dial := func(origin string, c *http.Cookie) (*websocket.Conn, *http.Response, error) {
		return websocket.DefaultDialer.Dial(wsURL, socketHeader(origin, c))
	}
	if _, res, err := dial(base, nil); err == nil || res == nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no session: %v %v", res, err)
	}
	cookie := signIn(t, s, base)
	if _, res, err := dial("https://evil.example", cookie); err == nil || res == nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("another page's socket was accepted: %v %v", res, err)
	}
	c, _, err := dial(base, cookie)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, msg := range []string{`{"id":1}`, `{"id":2,"text":"` + strings.Repeat("x", 9000) + `"}`} {
		if err := c.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
			t.Fatal(err)
		}
		_, got, err := c.ReadMessage()
		if err != nil || string(got) != msg {
			t.Fatalf("echo: %v, %d bytes for %d", err, len(got), len(msg))
		}
	}
	// turning the web UI off ends the connection and the session
	s.Disable()
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("the socket outlived Disable")
	}
	if err := s.Enable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, res, err := dial(base, cookie); err == nil || res == nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a session outlived Disable: %v %v", res, err)
	}
}

func TestGuessingDropsOutstandingCodes(t *testing.T) {
	s, base := start(t)
	open, _ := s.OpenURL()
	code := open[strings.Index(open, "#code=")+6:]
	for i := 0; i < maxFailures; i++ {
		do(t, "POST", base+"/api/session", map[string]string{"Origin": base}, `{"code":"WRONG`+strconv.Itoa(i)+`"}`)
	}
	if res := do(t, "POST", base+"/api/session", map[string]string{"Origin": base}, `{"code":"`+code+`"}`); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the code survived %d wrong guesses: %d", maxFailures, res.StatusCode)
	}
}

func testSheet(channel, id string) (Sheet, error) {
	switch {
	case channel == "c1" && id == "s1":
		return Sheet{Title: "A <fragment>", HTML: []byte(`<div class="card">hi</div>`)}, nil
	case channel == "c1" && id == "s2":
		return Sheet{Title: "doc", HTML: []byte("<!doctype html><html><HEAD lang=x><style>p{}</style></head><body>doc</body></html>")}, nil
	}
	return Sheet{}, errors.New("no such sheet")
}

// TestSheetsAreServedInert: a page an agent wrote needs a session to read,
// and arrives unable to do anything but draw.
func TestSheetsAreServedInert(t *testing.T) {
	s, base := start(t)
	if res := do(t, "GET", base+"/sheets/c1/s1", nil, ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a sheet without a session: %d", res.StatusCode)
	}
	cookie := signIn(t, s, base)
	auth := map[string]string{"Cookie": cookie.Name + "=" + cookie.Value}
	res := do(t, "GET", base+"/sheets/c1/s1", auth, "")
	body, _ := io.ReadAll(res.Body)
	csp := res.Header.Get("Content-Security-Policy")
	for _, want := range []string{"sandbox allow-scripts;", "default-src 'none'", "form-action 'none'", "frame-ancestors 'self'"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("csp lacks %q: %s", want, csp)
		}
	}
	for _, bad := range []string{"allow-same-origin", "allow-forms", "allow-popups", "allow-top-navigation", "connect-src", "'self' ", "*"} {
		if strings.Contains(csp, bad) {
			t.Fatalf("csp allows %q: %s", bad, csp)
		}
	}
	if res.StatusCode != 200 || !strings.Contains(string(body), `href="/sheet.css"`) || !strings.Contains(string(body), `<div class="card">hi</div>`) || !strings.Contains(string(body), "A &lt;fragment&gt;") {
		t.Fatalf("fragment: %d %s", res.StatusCode, body)
	}
	res = do(t, "GET", base+"/sheets/c1/s2", auth, "")
	body, _ = io.ReadAll(res.Body)
	if !strings.Contains(string(body), `<HEAD lang=x><meta charset="utf-8">`) || strings.Index(string(body), "sheet.css") > strings.Index(string(body), "<style>") {
		t.Fatalf("our stylesheet goes first in a document's own head: %s", body)
	}
	if res := do(t, "GET", base+"/sheets/c1/nope", auth, ""); res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown sheet: %d", res.StatusCode)
	}
	// the stylesheet is loadable from a sheet's opaque origin
	if res := do(t, "GET", base+"/sheet.css", nil, ""); res.Header.Get("Cross-Origin-Resource-Policy") != "cross-origin" {
		t.Fatalf("sheet.css CORP: %q", res.Header.Get("Cross-Origin-Resource-Policy"))
	}
}

// Signing out ends what the session had open, and only that.
func TestSigningOutEndsTheSessionsSockets(t *testing.T) {
	s, base := start(t)
	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/ws"
	open := func(c *http.Cookie) *websocket.Conn {
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, socketHeader(base, c))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	leaving, staying := signIn(t, s, base), signIn(t, s, base)
	gone, kept := open(leaving), open(staying)
	if res := do(t, "DELETE", base+"/api/session", map[string]string{"Origin": base, "Cookie": leaving.Name + "=" + leaving.Value}, ""); res.StatusCode != http.StatusNoContent {
		t.Fatalf("sign out: %d", res.StatusCode)
	}
	_ = gone.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := gone.ReadMessage(); err == nil || strings.Contains(err.Error(), "timeout") {
		t.Fatalf("the socket outlived its session: %v", err)
	}
	if err := kept.WriteMessage(websocket.TextMessage, []byte(`{"id":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, got, err := kept.ReadMessage(); err != nil || string(got) != `{"id":1}` {
		t.Fatalf("another session's socket was ended too: %v %s", err, got)
	}
}

// A sheet goes to the app's frame, not to a tab, a fetch or a script tag.
func TestASheetIsServedOnlyToAFrame(t *testing.T) {
	s, base := start(t)
	cookie := signIn(t, s, base)
	for dest, want := range map[string]int{"iframe": 200, "document": 403, "empty": 403, "script": 403, "image": 403} {
		res := do(t, "GET", base+"/sheets/c1/s1", map[string]string{"Cookie": cookie.Name + "=" + cookie.Value, "Sec-Fetch-Dest": dest}, "")
		if res.StatusCode != want {
			t.Errorf("Sec-Fetch-Dest %s: %d, want %d", dest, res.StatusCode, want)
		}
	}
}

// A cookie goes to every port of a host, so another server on this machine's
// loopback receives it and could replay it here. It is not enough: the socket
// wants the page key too, which no browser sends anywhere by itself.
func TestTheCookieAloneOpensNothing(t *testing.T) {
	s, base := start(t)
	cookie := signIn(t, s, base)
	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/ws"
	stolen := http.Header{"Origin": {base}, "Cookie": {cookie.Name + "=" + cookie.Value}}
	if _, res, err := websocket.DefaultDialer.Dial(wsURL, stolen); err == nil || res == nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a socket opened on the cookie alone: %v %v", res, err)
	}
	stolen.Set("Sec-WebSocket-Protocol", "stavlos, key.guess")
	if _, res, err := websocket.DefaultDialer.Dial(wsURL, stolen); err == nil || res == nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a socket opened on a guessed key: %v %v", res, err)
	}
	if res := do(t, "GET", base+"/api/session", map[string]string{"Cookie": cookie.Name + "=" + cookie.Value}, ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the page was told it is signed in without its key: %d", res.StatusCode)
	}
	c, _, err := websocket.DefaultDialer.Dial(wsURL, socketHeader(base, cookie))
	if err != nil {
		t.Fatalf("the page itself could not open its socket: %v", err)
	}
	c.Close()
}
