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

// URL is the models.dev database endpoint.
const URL = "https://models.dev/api.json"

// TTL is how long a cached copy is considered fresh.
const TTL = 24 * time.Hour

// fetchTimeout bounds the network fetch so a slow endpoint cannot stall startup.
const fetchTimeout = 15 * time.Second

// CachePath is where the fetched database is stored.
func CachePath() string { return filepath.Join(paths.CacheDir(), "models.json") }

// Load returns the catalog, preferring in order: a fresh cache, the network,
// a stale cache, and finally the embedded fallback. It never fails on a
// network error alone; an error is returned only if nothing parses.
func Load(ctx context.Context) (*Catalog, error) {
	path := CachePath()

	if data, fresh := readCache(path); fresh {
		if c, err := Parse(data); err == nil {
			return c, nil
		}
	}

	data, fetchErr := fetch(ctx)
	if fetchErr == nil {
		if c, err := Parse(data); err == nil {
			writeCache(path, data) // best effort
			return c, nil
		} else {
			fetchErr = err
		}
	}

	if data, _ := readCache(path); data != nil {
		if c, err := Parse(data); err == nil {
			return c, nil
		}
	}

	c, err := Parse(fallbackJSON)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("modelsdev: fetch: %w", fetchErr), err)
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

func writeCache(path string, data []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
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
