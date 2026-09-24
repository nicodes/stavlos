package web

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"html"
	"net/http"
	"regexp"
)

// Sheet is an HTML page an agent wrote (docs/web-ui.md).
type Sheet struct {
	Title string
	HTML  []byte
}

// sheetCSS is the stylesheet every sheet gets: Tailwind utilities and
// daisyUI components, built with the client and served, never fetched.
const sheetCSS = "sheet.css"

// sheet serves an agent-written page as something that can do little but
// draw. The sandbox directive gives the document an opaque origin even when
// its URL is opened in a tab of its own, so it is never same-origin with the
// app: it cannot read the app's DOM or storage, and the session cookie is
// HttpOnly. Nothing loads from anywhere but the page itself and our
// stylesheet (default-src 'none': no fetch, no remote image, script or
// font), forms and popups are off, and it may be framed by the app alone.
// What no policy stops is the frame navigating itself to a URL of its own
// (and WebRTC), which can carry data out when the human opens the tab: so
// writing a sheet asks, like a fetch does (config.Defaults).
func (s *Server) sheet(w http.ResponseWriter, r *http.Request) {
	if !s.signedIn(r) {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	// The cookie alone opens nothing (server.go): a frame cannot carry the
	// page key in a header, so it carries a token made from it for this one
	// sheet, which the page can compute and another server on this
	// machine's loopback, holding a replayed cookie, cannot.
	if !s.holdsSheetToken(r, r.PathValue("channel"), r.PathValue("id")) {
		http.Error(w, "a sheet is opened by the app", http.StatusForbidden)
		return
	}
	// A browser says what a request is for. A sheet is for the app's frame and
	// nothing else: not a tab of its own, not a fetch, not another page's
	// image or script tag. (A client that does not say is not a browser.)
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest != "" && dest != "iframe" {
		http.Error(w, "a sheet is shown inside the app", http.StatusForbidden)
		return
	}
	if s.o.Sheet == nil {
		http.NotFound(w, r)
		return
	}
	sh, err := s.o.Sheet(r.PathValue("channel"), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	css := "http://" + r.Host + "/" + sheetCSS + " https://" + r.Host + "/" + sheetCSS
	h := w.Header()
	h.Set("Content-Security-Policy", "sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline' "+css+
		"; img-src data: blob:; font-src data:; media-src data: blob:; base-uri 'none'; form-action 'none'; frame-ancestors 'self'")
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Del("Cross-Origin-Opener-Policy")
	_, _ = w.Write(framed(sh))
}

// holdsSheetToken checks the token a sheet's URL carries: an HMAC over the
// channel and sheet id, keyed with the hash of the page key (what the
// server keeps of it, and what the page computes from what it holds).
func (s *Server) holdsSheetToken(r *http.Request, channel, id string) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	s.mu.Lock()
	ses, ok := s.sessions[sha256.Sum256([]byte(c.Value))]
	s.mu.Unlock()
	if !ok {
		return false
	}
	want := sheetToken(ses.key, channel, id)
	return subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("t")), []byte(want)) == 1
}

// sheetToken is the token for one sheet under a session, hex.
func sheetToken(keyHash [32]byte, channel, id string) string {
	m := hmac.New(sha256.New, keyHash[:])
	m.Write([]byte(channel + "\n" + id))
	return hex.EncodeToString(m.Sum(nil))
}

var (
	headOpen = regexp.MustCompile(`(?i)<head[^>]*>`)
	htmlOpen = regexp.MustCompile(`(?i)<html[^>]*>`)
)

// framed puts our stylesheet in front of the page's own styles: into its
// <head> when it is a document, around it when it is a fragment.
func framed(sh Sheet) []byte {
	link := []byte(`<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><link rel="stylesheet" href="/` + sheetCSS + `">`)
	if loc := headOpen.FindIndex(sh.HTML); loc != nil {
		return bytes.Join([][]byte{sh.HTML[:loc[1]], link, sh.HTML[loc[1]:]}, nil)
	}
	if loc := htmlOpen.FindIndex(sh.HTML); loc != nil {
		return bytes.Join([][]byte{sh.HTML[:loc[1]], []byte("<head>"), link, []byte("</head>"), sh.HTML[loc[1]:]}, nil)
	}
	var b bytes.Buffer
	b.WriteString(`<!doctype html><html data-theme="dark"><head>`)
	b.Write(link)
	b.WriteString("<title>" + html.EscapeString(sh.Title) + `</title></head><body class="p-6">`)
	b.Write(sh.HTML)
	b.WriteString("</body></html>")
	return b.Bytes()
}
