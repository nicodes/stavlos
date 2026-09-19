package web

import (
	"bytes"
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

// sheet serves an agent-written page as something that can do nothing but
// draw. The sandbox directive gives the document an opaque origin even when
// its URL is opened in a tab of its own, so it is never same-origin with the
// app: it cannot read the app's DOM or storage, and the session cookie is
// HttpOnly. Nothing loads from anywhere but the page itself and our
// stylesheet (default-src 'none': no fetch, no remote image, script, font or
// frame that could carry data out), forms and popups are off, and it may be
// framed by the app alone.
func (s *Server) sheet(w http.ResponseWriter, r *http.Request) {
	if !s.signedIn(r) {
		http.Error(w, "not signed in", http.StatusUnauthorized)
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
