package modelsdev

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/nicodes/stavlos/internal/httpx"
	"github.com/nicodes/stavlos/internal/paths"
	"github.com/nicodes/stavlos/internal/statefile"
)

// URL is the models.dev database endpoint (a variable for tests).
var URL = "https://models.dev/api.json"

// TTL is how long a cached copy is considered fresh.
const TTL = 24 * time.Hour

// fetchTimeout bounds one network fetch.
const fetchTimeout = 15 * time.Second

// CachePath is where the fetched database is stored.
func CachePath() string { return filepath.Join(paths.CacheDir(), "models.json") }

// Load returns the catalog without waiting on the network when it can: a
// fresh cache, else a stale cache, else the network, else the embedded
// copy. stale reports that the caller should Refresh in the background (a
// stale cache or the embedded fallback was returned). An error is returned
// only if nothing parses. keep names the providers to keep (Parse).
func Load(ctx context.Context, keep ...string) (c *Catalog, stale bool, err error) {
	var cached *Catalog
	if data, fresh := readCache(CachePath()); data != nil {
		if c, err := Parse(data, keep...); err == nil {
			// The cache holds only the providers the build that wrote it
			// kept. One written before a provider existed is missing it
			// however recent the file is, and serving it would offer that
			// subscription no models at all, with nothing to say why.
			if c.covers(keep...) {
				return c, !fresh, nil
			}
			cached = c
		}
	}
	c, fetchErr := Refresh(ctx, keep...)
	if fetchErr == nil {
		return c, false, nil
	}
	if cached != nil {
		return cached, true, nil // incomplete, but better than the embedded copy
	}
	c, err = Parse(fallbackJSON, keep...)
	if err != nil {
		return nil, false, errors.Join(fmt.Errorf("modelsdev: fetch: %w", fetchErr), err)
	}
	return c, true, nil
}

// Refresh fetches the database, parses the providers in keep and caches
// only what it kept, so the next start reads kilobytes, not megabytes.
func Refresh(ctx context.Context, keep ...string) (*Catalog, error) {
	data, err := fetch(ctx)
	if err != nil {
		return nil, err
	}
	c, err := Parse(data, keep...)
	if err != nil {
		return nil, err
	}
	if b, err := c.encode(); err == nil {
		writeCache(CachePath(), b) // best effort
	}
	return c, nil
}

// readCache returns the cached bytes (nil if absent) and whether they are fresh.
func readCache(path string) ([]byte, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	return data, time.Since(st.ModTime()) < TTL
}

// writeCache replaces the cache through a temporary file of its own, so two
// processes refreshing at once never interleave their writes.
func writeCache(path string, data []byte) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	_ = statefile.WriteAtomic(path, data, 0o600, false) // a cache: the next start fetches again
}

// client bounds itself by the request's context.
var client = httpx.New(httpx.Options{HeaderTimeout: fetchTimeout})

func fetch(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "stavlos")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", URL, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}
