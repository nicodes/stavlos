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

	"github.com/nicodes/stavlos/internal/paths"
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
	path := CachePath()
	if data, fresh := readCache(path); data != nil {
		if c, err := Parse(data, keep...); err == nil {
			return c, !fresh, nil
		}
	}
	c, fetchErr := Refresh(ctx, keep...)
	if fetchErr == nil {
		return c, false, nil
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
	f, err := os.CreateTemp(dir, "models-*.json")
	if err != nil {
		return
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
	}
}

func fetch(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "stavlos")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", URL, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}
