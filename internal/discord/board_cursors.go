package discord

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/nicodes/stavlos/internal/statefile"
)

// Board cursors are separate from prompt controls; ordinary channel replay
// remains unchanged. The Discord channel id scopes a cursor to that mirror.
type boardCursors struct {
	mu   sync.Mutex
	path string
	seq  map[string]int64
}

func openBoardCursors(path string) (*boardCursors, error) {
	c := &boardCursors{path: path, seq: map[string]int64{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &c.seq); err != nil {
		return nil, err
	}
	if c.seq == nil {
		c.seq = map[string]int64{}
	}
	return c, nil
}
func (c *boardCursors) get(id string) int64 { c.mu.Lock(); defer c.mu.Unlock(); return c.seq[id] }
func (c *boardCursors) set(id string, seq int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	old := c.seq[id]
	c.seq[id] = seq
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		c.seq[id] = old
		return err
	}
	b, err := json.Marshal(c.seq)
	if err == nil {
		err = statefile.WriteAtomic(c.path, b, 0o600, false)
	}
	if err != nil {
		c.seq[id] = old
	}
	return err
}
