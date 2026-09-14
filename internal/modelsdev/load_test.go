package modelsdev

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const tiny = `{"openai":{"id":"openai","models":{"gpt-x":{"id":"gpt-x","limit":{"context":1000}}}}}`

func withCache(t *testing.T, network http.HandlerFunc) {
	t.Helper()
	t.Setenv("STAVLOS_CACHE_DIR", t.TempDir())
	srv := httptest.NewServer(network)
	t.Cleanup(srv.Close)
	old := URL
	URL = srv.URL
	t.Cleanup(func() { URL = old })
}

func unreachable(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "down", http.StatusServiceUnavailable)
}

// TestLoadPrefersTheCache: a fresh cache is used as is; a stale one is used
// at once and reported stale, without waiting on the network.
func TestLoadPrefersTheCache(t *testing.T) {
	hits := 0
	withCache(t, func(w http.ResponseWriter, r *http.Request) { hits++; unreachable(w, r) })
	path := CachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(tiny), 0o644); err != nil {
		t.Fatal(err)
	}
	c, stale, err := Load(context.Background())
	if err != nil || stale || len(c.Models("openai")) != 1 || hits != 0 {
		t.Fatalf("fresh: stale %v err %v hits %d", stale, err, hits)
	}
	old := time.Now().Add(-2 * TTL)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	c, stale, err = Load(context.Background())
	if err != nil || !stale || len(c.Models("openai")) != 1 || hits != 0 {
		t.Fatalf("stale: stale %v err %v hits %d", stale, err, hits)
	}
}

// TestLoadWithoutCache: the network when it answers (and the answer is
// cached), else the embedded copy, reported stale.
func TestLoadWithoutCache(t *testing.T) {
	withCache(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tiny)) })
	c, stale, err := Load(context.Background())
	if err != nil || stale || len(c.Models("openai")) != 1 {
		t.Fatalf("network: stale %v err %v", stale, err)
	}
	if data, fresh := readCache(CachePath()); !fresh || string(data) != tiny {
		t.Fatalf("not cached: fresh %v %q", fresh, data)
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(CachePath()), "models-*.json"))
	if len(matches) != 0 {
		t.Fatalf("temporary files left: %v", matches)
	}

	withCache(t, unreachable)
	c, stale, err = Load(context.Background())
	if err != nil || !stale || len(c.Models("openai")) == 0 {
		t.Fatalf("embedded: stale %v err %v", stale, err)
	}
	if _, err := Refresh(context.Background()); err == nil {
		t.Fatal("refresh against a failing server should fail")
	}
}
