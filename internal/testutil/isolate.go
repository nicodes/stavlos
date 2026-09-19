package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// Dirs are a test's own Stavlos directories.
type Dirs struct{ Config, Data, Cache, Socket string }

// Isolate gives a test its own config, data and cache directories and socket
// path, removed when it ends, and points the process at them. It replaces
// the groups of t.Setenv("STAVLOS_…") lines tests wrote for themselves, most
// of which set the config directory and forgot the data directory.
// (internal/paths also refuses the user's own directories to any test
// binary; this is for tests that need to know where theirs are.)
//
// The socket lives in a short directory of its own: a test's temp directory
// under a long GOTMPDIR with a long test name passes the ~100 bytes a Unix
// socket path may have.
func Isolate(t testing.TB) Dirs {
	t.Helper()
	d := Dirs{Config: t.TempDir(), Data: t.TempDir(), Cache: t.TempDir()}
	short, err := os.MkdirTemp("/tmp", "sv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	d.Socket = filepath.Join(short, "s.sock")
	t.Setenv("STAVLOS_CONFIG_DIR", d.Config)
	t.Setenv("STAVLOS_DATA_DIR", d.Data)
	t.Setenv("STAVLOS_CACHE_DIR", d.Cache)
	t.Setenv("STAVLOS_SOCKET", d.Socket)
	return d
}
