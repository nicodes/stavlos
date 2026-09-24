package daemon

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/nicodes/stavlos/internal/paths"
)

// A tool output over the cap is kept whole under <cache>/tmp/<channel>/output
// (tools.Env.Clip), named in the result for the agent to read a part of.
// Nothing read it after a few days, and nothing deleted it either: the
// directory grew for as long as the install lived. OpenCode keeps its
// equivalent for seven days; so does this.

const (
	overflowKeep  = 7 * 24 * time.Hour
	overflowEvery = 6 * time.Hour
)

// sweepOverflow deletes overflow files older than overflowKeep, now and
// every overflowEvery, until ctx ends.
func sweepOverflow(ctx context.Context) {
	root := filepath.Join(paths.CacheDir(), "tmp")
	for {
		if n, err := sweepOlder(root, time.Now().Add(-overflowKeep)); err != nil {
			log.Printf("overflow sweep: %v", err)
		} else if n > 0 {
			log.Printf("overflow sweep: removed %d files older than %s", n, overflowKeep)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(overflowEvery):
		}
	}
}

// sweepOlder removes every file under root/*/output modified before cutoff
// and reports how many. A missing root is nothing to do.
func sweepOlder(root string, cutoff time.Time) (int, error) {
	dirs, err := filepath.Glob(filepath.Join(root, "*", "output"))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			info, err := e.Info()
			if err != nil || e.IsDir() || !info.ModTime().Before(cutoff) {
				continue
			}
			if os.Remove(filepath.Join(dir, e.Name())) == nil {
				n++
			}
		}
		if rest, err := os.ReadDir(dir); err == nil && len(rest) == 0 {
			_ = os.Remove(dir)
		}
	}
	return n, nil
}
