package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A credential is sent to the host that was named and to no other: a
// redirect on that host is followed, one that leaves it is refused.
func TestARedirectStaysOnTheHostThatWasAsked(t *testing.T) {
	var elsewhere int
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere++ }))
	defer other.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/here", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/there", http.StatusFound) })
	mux.HandleFunc("/there", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "secret" {
			t.Error("the key did not follow a redirect on its own host")
		}
	})
	mux.HandleFunc("/away", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) })
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/loop", http.StatusFound) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	get := func(path string) error {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Header.Set("X-Api-Key", "secret")
		resp, err := New(Options{}).Do(req)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := get("/here"); err != nil {
		t.Errorf("a redirect on the same host: %v", err)
	}
	if err := get("/away"); err == nil || !strings.Contains(err.Error(), "refused a redirect") {
		t.Errorf("a redirect to another host: %v", err)
	}
	if elsewhere != 0 {
		t.Errorf("the other host was called %d times", elsewhere)
	}
	if err := get("/loop"); err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Errorf("an endless redirect: %v", err)
	}
}
