// Package navigation remembers the human's last viewed channel, independently
// of the daemon's activity and of the directory a client was launched from.
package navigation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nicodes/stavlos/internal/paths"
)

type state struct {
	Channel string `json:"channel"`
	Viewed  int64  `json:"viewed"`
}

var mu sync.Mutex

func filename() string { return filepath.Join(paths.DataDir(), "navigation.json") }

// Last returns the last viewed channel, or empty if no usable state exists.
func Last() string { return load().Channel }

func load() state {
	var s state
	if b, err := os.ReadFile(filename()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

// Remember atomically saves a successful selection. The selection timestamp
// prevents a delayed UI command from overwriting a newer selection.
func Remember(channel string, selected time.Time) error {
	mu.Lock()
	defer mu.Unlock()
	if channel == "" || load().Viewed > selected.UnixNano() {
		return nil
	}
	if err := os.MkdirAll(paths.DataDir(), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(state{channel, selected.UnixNano()})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(paths.DataDir(), ".navigation-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	if ce := f.Close(); err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), filename())
}
