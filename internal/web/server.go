// Package web is the daemon's browser front door (docs/web-ui.md): one
// loopback listener that serves the embedded client and carries the JSON-RPC
// protocol over a WebSocket. It never listens off loopback; reaching it from
// another device is Tailscale's or an SSH tunnel's job.
//
// Three checks stand in front of everything it serves. The Host header must
// name this listener (or a host the configuration lists, for a proxy such as
// `tailscale serve`), which is what defeats DNS rebinding. A request that
// changes anything, and the WebSocket upgrade, must carry an Origin that
// names it too, which is what stops another page in the same browser. And
// the session is an HttpOnly, SameSite=Strict cookie, traded once for a
// one-time code the TUI hands out, so no token travels in a URL the server
// sees.
package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// DefaultPort is where the web UI listens unless the configuration says
// otherwise. It is fixed, not ephemeral, so a tunnel or `tailscale serve`
// set up once keeps working across daemon restarts.
const DefaultPort = 4999

const (
	cookieName  = "stavlos_session"
	codeTTL     = 2 * time.Minute
	sessionTTL  = 30 * 24 * time.Hour
	maxFailures = 10      // wrong codes before every outstanding code is dropped
	maxMessage  = 4 << 20 // one protocol request, like the socket's line bound
)

// Options configure a Server.
type Options struct {
	Port  int      // 0 = DefaultPort
	Hosts []string // more names the listener answers to ("box.tailnet.ts.net"), beside loopback
	// Serve runs the protocol on one authenticated connection until it
	// closes. The connection carries newline-delimited JSON-RPC.
	Serve func(context.Context, net.Conn)
	// Sheet returns one of a channel's sheets, an HTML page an agent wrote.
	// nil serves none.
	Sheet func(channel, id string) (Sheet, error)
	// Assets is the built client; nil serves the embedded one.
	Assets fs.FS
}

// Status is what the server reports about itself.
type Status struct {
	Enabled bool
	URL     string
	Error   string
}

// Server is the web listener. The zero state is disabled.
type Server struct {
	o Options

	mu       sync.Mutex
	ln       net.Listener
	srv      *http.Server
	cancel   context.CancelFunc
	lastErr  string
	codes    map[[32]byte]time.Time // one-time codes by hash → expiry
	sessions map[[32]byte]session   // session tokens by hash
	failures int
	// sockets are the open connections of each session, so that signing a
	// session out ends them: a cookie that is gone must not leave a live
	// stream behind it.
	sockets map[[32]byte]map[*context.CancelFunc]struct{}
}

// session is one signed-in browser. It is held by two secrets that travel
// differently. The cookie is HttpOnly, so no script reads it; but a cookie is
// scoped to a host and not to a port, so a browser sends it to anything else
// served on this machine's loopback (a dev server on :3000) and that server
// could replay it here. The page key lives in the page's localStorage, which
// is scoped to the origin, port included, and no browser sends it anywhere
// by itself. The channel to the daemon needs both.
type session struct {
	exp time.Time
	key [32]byte // hash of the page key
}

// New returns a disabled Server.
func New(o Options) *Server {
	if o.Port == 0 {
		o.Port = DefaultPort
	}
	if o.Assets == nil {
		o.Assets = embedded()
	}
	return &Server{o: o, codes: map[[32]byte]time.Time{}, sessions: map[[32]byte]session{}, sockets: map[[32]byte]map[*context.CancelFunc]struct{}{}}
}

// URL is the address of the client on this machine.
func (s *Server) URL() string { return "http://127.0.0.1:" + strconv.Itoa(s.o.Port) + "/" }

// Status reports whether the listener is up.
func (s *Server) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{Enabled: s.ln != nil, URL: s.URL(), Error: s.lastErr}
}

// Enable starts listening on loopback. Enabling twice is not an error.
func (s *Server) Enable(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		return nil
	}
	ln, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(s.o.Port)))
	if err != nil {
		s.lastErr = err.Error()
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    64 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ErrorLog:          log.New(logWriter{}, "web: ", 0),
	}
	s.ln, s.srv, s.cancel, s.lastErr = ln, srv, cancel, ""
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("web: %v", err)
		}
	}()
	go func() { // the daemon stopping stops the listener
		<-ctx.Done()
		_ = srv.Close()
	}()
	return nil
}

// Disable stops the listener, closes every connection and forgets every
// session: turning the web UI off signs every browser out.
func (s *Server) Disable() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return
	}
	s.cancel() // ends the WebSocket connections, which http.Server.Close does not track
	_ = s.srv.Close()
	s.ln, s.srv, s.cancel = nil, nil, nil
	clear(s.codes)
	clear(s.sessions)
	clear(s.sockets)
}

// OpenURL is the client's address carrying a fresh one-time code in its
// fragment, which a browser never sends to a server or writes to a log. The
// page trades the code for its session cookie and drops the fragment.
func (s *Server) OpenURL() (string, error) {
	code, err := randomToken(10)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return "", errors.New("the web UI is disabled")
	}
	s.sweepLocked(time.Now())
	s.codes[sha256.Sum256([]byte(code))] = time.Now().Add(codeTTL)
	return s.URL() + "#code=" + code, nil
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), nil
}

func (s *Server) sweepLocked(now time.Time) {
	for k, exp := range s.codes {
		if now.After(exp) {
			delete(s.codes, k)
		}
	}
	for k, ses := range s.sessions {
		if now.After(ses.exp) {
			delete(s.sessions, k)
		}
	}
}

// redeem trades a one-time code for a session token.
func (s *Server) redeem(code string) (token, pageKey string, ok bool) {
	key := sha256.Sum256([]byte(strings.ToUpper(strings.TrimSpace(code))))
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(now)
	found := false
	for k := range s.codes { // constant time over what is outstanding
		if subtle.ConstantTimeCompare(k[:], key[:]) == 1 {
			found = true
		}
	}
	if !found {
		if s.failures++; s.failures >= maxFailures {
			clear(s.codes) // guessing costs everyone their code, never yields one
			s.failures = 0
		}
		return "", "", false
	}
	delete(s.codes, key)
	s.failures = 0
	token, err := randomToken(32)
	if err != nil {
		return "", "", false
	}
	if pageKey, err = randomToken(32); err != nil {
		return "", "", false
	}
	s.sessions[sha256.Sum256([]byte(token))] = session{exp: now.Add(sessionTTL), key: sha256.Sum256([]byte(pageKey))}
	return token, pageKey, true
}

func (s *Server) signedIn(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return false
	}
	key := sha256.Sum256([]byte(c.Value))
	s.mu.Lock()
	defer s.mu.Unlock()
	ses, ok := s.sessions[key]
	return ok && time.Now().Before(ses.exp)
}

// keyHeader carries the page key on a fetch; a WebSocket, which cannot set
// headers, offers it as a subprotocol beside wsProtocol.
const (
	keyHeader   = "X-Stavlos-Key"
	wsProtocol  = "stavlos"
	wsKeyPrefix = "key."
)

// holdsKey reports whether the request carries its session's page key.
func (s *Server) holdsKey(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	offered := r.Header.Get(keyHeader)
	for _, p := range websocket.Subprotocols(r) {
		if rest, ok := strings.CutPrefix(p, wsKeyPrefix); ok {
			offered = rest
		}
	}
	got := sha256.Sum256([]byte(offered))
	s.mu.Lock()
	defer s.mu.Unlock()
	ses, ok := s.sessions[sha256.Sum256([]byte(c.Value))]
	return ok && offered != "" && subtle.ConstantTimeCompare(ses.key[:], got[:]) == 1
}

func (s *Server) signOut(r *http.Request) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return
	}
	key := sha256.Sum256([]byte(c.Value))
	s.mu.Lock()
	delete(s.sessions, key)
	open := s.sockets[key]
	delete(s.sockets, key)
	s.mu.Unlock()
	for cancel := range open {
		(*cancel)()
	}
}

// hold ties a connection to the session that opened it until release; it
// reports false when the session ended in the meantime.
func (s *Server) hold(r *http.Request, cancel *context.CancelFunc) (release func(), ok bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil, false
	}
	key := sha256.Sum256([]byte(c.Value))
	s.mu.Lock()
	defer s.mu.Unlock()
	if ses, live := s.sessions[key]; !live || !time.Now().Before(ses.exp) {
		return nil, false
	}
	if s.sockets[key] == nil {
		s.sockets[key] = map[*context.CancelFunc]struct{}{}
	}
	s.sockets[key][cancel] = struct{}{}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.sockets[key], cancel)
		if len(s.sockets[key]) == 0 {
			delete(s.sockets, key)
		}
	}, true
}

// --- what may talk to us ---

// hostAllowed reports whether a Host header (or an Origin's host) names this
// listener: loopback on our port, or a configured host on any port.
func (s *Server) hostAllowed(hostport string) bool {
	hostport = strings.ToLower(hostport)
	port := strconv.Itoa(s.o.Port)
	for _, h := range []string{"127.0.0.1", "localhost"} {
		if hostport == h+":"+port {
			return true
		}
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	for _, h := range s.o.Hosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" && (host == h || hostport == h) {
			return true
		}
	}
	return false
}

// originAllowed reports whether the request says it comes from our own page.
// A request without an Origin is refused: every browser sends one on a POST
// and on a WebSocket upgrade, and nothing else has a reason to call these.
func (s *Server) originAllowed(r *http.Request) bool {
	u, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := u.Host
	if u.Port() == "" && isLoopbackName(u.Hostname()) {
		return false // loopback always carries our port
	}
	return s.hostAllowed(host)
}

func isLoopbackName(h string) bool { return h == "127.0.0.1" || h == "localhost" }

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /api/session", s.sessionGet)
	mux.HandleFunc("POST /api/session", s.sessionPost)
	mux.HandleFunc("DELETE /api/session", s.sessionDelete)
	mux.HandleFunc("GET /ws", s.ws)
	mux.HandleFunc("GET /sheets/{channel}/{id}", s.sheet)
	mux.HandleFunc("GET /", s.asset)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			http.Error(w, "this name is not one the Stavlos web UI answers to; list it under web.hosts in stavlos.json", http.StatusMisdirectedRequest)
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) sessionGet(w http.ResponseWriter, r *http.Request) {
	if !s.signedIn(r) || !s.holdsKey(r) {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) sessionPost(w http.ResponseWriter, r *http.Request) {
	if !s.originAllowed(r) {
		http.Error(w, "origin refused", http.StatusForbidden)
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	token, pageKey, ok := s.redeem(body.Code)
	if !ok {
		http.Error(w, "that code is wrong, used or expired; open the web UI from Stavlos again", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/", MaxAge: int(sessionTTL / time.Second),
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: strings.HasPrefix(r.Header.Get("Origin"), "https://"), // behind a TLS proxy such as tailscale serve
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{"key": pageKey})
}

func (s *Server) sessionDelete(w http.ResponseWriter, r *http.Request) {
	if !s.originAllowed(r) {
		http.Error(w, "origin refused", http.StatusForbidden)
		return
	}
	s.signOut(r)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

// ws upgrades an authenticated, same-origin request and hands the
// connection to the protocol.
func (s *Server) ws(w http.ResponseWriter, r *http.Request) {
	if !s.signedIn(r) || !s.holdsKey(r) {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	up := websocket.Upgrader{Subprotocols: []string{wsProtocol}, CheckOrigin: s.originAllowed, ReadBufferSize: 4 << 10, WriteBufferSize: 16 << 10}
	c, err := up.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade has replied
	}
	c.SetReadLimit(maxMessage)
	nc := newWSConn(c)
	defer nc.Close()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	release, ok := s.hold(r, &cancel)
	if !ok {
		return // signed out between the check and the upgrade
	}
	defer release()
	go func() { // the listener closing, Disable, or signing out ends the connection
		<-ctx.Done()
		nc.Close()
	}()
	s.o.Serve(ctx, nc)
}

// asset serves the built client; a path that is no file is the app's own
// route and gets index.html.
func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	if st, err := fs.Stat(s.o.Assets, name); err != nil || st.IsDir() {
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		name = "index.html"
	}
	ws := "ws://" + r.Host + " wss://" + r.Host // 'self' covers these in current browsers; older ones need them spelled out
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; "+
			"connect-src 'self' "+ws+"; manifest-src 'self'; frame-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	if name == sheetCSS {
		// A sheet's document has an opaque origin, so its stylesheet is a
		// cross-origin load; it holds nothing private.
		w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable") // content-hashed by the build
	}
	http.ServeFileFS(w, r, s.o.Assets, name)
}

type logWriter struct{}

func (logWriter) Write(p []byte) (int, error) {
	log.Print(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
