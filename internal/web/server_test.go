package web

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

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
	s := New(Options{Port: freePort(t), Hosts: hosts, Serve: echoLines,
		Assets: fstest.MapFS{"index.html": {Data: []byte("<!doctype html>app")}, "assets/app.js": {Data: []byte("1")}}})
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

func signIn(t *testing.T, s *Server, base string) *http.Cookie {
	t.Helper()
	open, err := s.OpenURL()
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(open)
	code := strings.TrimPrefix(u.Fragment, "code=")
	res := do(t, "POST", base+"/api/session", map[string]string{"Origin": base}, `{"code":"`+code+`"}`)
	if res.StatusCode != http.StatusNoContent || len(res.Cookies()) != 1 {
		t.Fatalf("sign-in: %d %v", res.StatusCode, res.Cookies())
	}
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
	if res.StatusCode != http.StatusNoContent {
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
		h := http.Header{"Origin": {origin}}
		if c != nil {
			h.Set("Cookie", c.Name+"="+c.Value)
		}
		return websocket.DefaultDialer.Dial(wsURL, h)
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
