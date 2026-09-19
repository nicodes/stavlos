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
	if data, fresh := readCache(CachePath()); !fresh || data == nil {
		t.Fatalf("not cached: fresh %v %q", fresh, data)
	} else if cached, err := Parse(data); err != nil || len(cached.Models("openai")) != 1 {
		t.Fatalf("the cache does not hold the catalog: %v %q", err, data)
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

// TestLoadRefetchesForAProviderTheCacheLacks: the cache holds only what the
// build that wrote it kept, so adding a provider must not be answered from
// a cache that predates it — otherwise the new subscription offers no
// models until the cache expires, with nothing to say why.
func TestLoadRefetchesForAProviderTheCacheLacks(t *testing.T) {
	const both = `{"openai":{"id":"openai","models":{"gpt-x":{"id":"gpt-x","limit":{"context":1000}}}},
	               "zai-coding-plan":{"id":"zai-coding-plan","models":{"glm-x":{"id":"glm-x","limit":{"context":2000}}}}}`
	hits := 0
	withCache(t, func(w http.ResponseWriter, _ *http.Request) { hits++; _, _ = w.Write([]byte(both)) })
	path := CachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(tiny), 0o644); err != nil { // an older build's cache: openai only
		t.Fatal(err)
	}
	c, stale, err := Load(context.Background(), "openai", "zai-coding-plan")
	if err != nil || stale || hits != 1 {
		t.Fatalf("a cache missing a provider is refetched: stale %v err %v hits %d", stale, err, hits)
	}
	if len(c.Models("zai-coding-plan")) != 1 {
		t.Fatalf("the new provider's models: %v", c.Models("zai-coding-plan"))
	}
	// and the refetched copy is cached, so the next start is not another fetch
	c, stale, err = Load(context.Background(), "openai", "zai-coding-plan")
	if err != nil || stale || hits != 1 || len(c.Models("zai-coding-plan")) != 1 {
		t.Fatalf("second load: stale %v err %v hits %d", stale, err, hits)
	}
}

// TestLoadKeepsAnIncompleteCacheWhenOffline: an incomplete cache still
// beats the embedded copy when the fetch it triggers fails.
func TestLoadKeepsAnIncompleteCacheWhenOffline(t *testing.T) {
	hits := 0
	withCache(t, func(w http.ResponseWriter, r *http.Request) { hits++; unreachable(w, r) })
	path := CachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(tiny), 0o644); err != nil {
		t.Fatal(err)
	}
	c, stale, err := Load(context.Background(), "openai", "zai-coding-plan")
	if err != nil || !stale || hits != 1 {
		t.Fatalf("offline: stale %v err %v hits %d", stale, err, hits)
	}
	if len(c.Models("openai")) != 1 || len(c.Models("zai-coding-plan")) != 0 {
		t.Fatal("the cache is served as it is, missing provider and all")
	}
}
