package config

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/nicodes/stavlos/internal/paths"
	"github.com/nicodes/stavlos/internal/statefile"
)

// globalWriteMu serialises every writer of the global configuration
// directory, so two settings changed at once both land.
var globalWriteMu sync.Mutex

// editGlobalConfig changes the global stavlos.json in one step: it takes the
// write lock, reads the file (an empty object when it does not exist and
// create is set; an error otherwise), hands the bytes to edit, and writes the
// result back atomically. A symlinked stavlos.json stays a symlink: the
// bytes go to its target. There used to be three writers, and one of them
// replaced the link with a regular file.
func editGlobalConfig(create bool, edit func([]byte) ([]byte, error)) error {
	globalWriteMu.Lock()
	defer globalWriteMu.Unlock()
	p := filepath.Join(paths.ConfigDir(), "stavlos.json")
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		if !create {
			return err
		}
		b = []byte("{}\n")
	} else if err != nil {
		return err
	}
	out, err := edit(b)
	if err != nil {
		return err
	}
	if bytes.Equal(b, out) {
		return nil
	}
	return writeConfigFile(p, out, 0o600, false) // private: it may hold a token or a search key
}

// writeConfigFile writes a configuration file atomically, through its symlink
// when it is one: the link stays, its target holds the new bytes. The parent
// directory of the path is created when missing.
func writeConfigFile(path string, data []byte, mode os.FileMode, keepMode bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if st, err := os.Lstat(path); err == nil && st.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		path = resolved
	}
	return statefile.WriteAtomic(path, data, mode, keepMode)
}
