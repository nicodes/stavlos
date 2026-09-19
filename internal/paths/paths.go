// Package paths centralises filesystem locations (PRD §10.1, §10.7, §11.3).
package paths

import (
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

func xdg(env, fallback string) string {
	if underTest() {
		// A test never reaches the user's own config, data or cache, even
		// one that forgot to say where its own are: 58 t.Setenv lines in 25
		// files is 58 chances to forget, and the code under test writes
		// sheets, state files and an event log.
		return filepath.Join(os.TempDir(), "stavlos-test-"+strconv.Itoa(os.Getpid()), fallback)
	}
	if v := os.Getenv(env); v != "" {
		return v
	}
	return filepath.Join(home(), fallback)
}

// underTest reports whether this process is a test binary.
func underTest() bool {
	return strings.HasSuffix(os.Args[0], ".test") || flag.Lookup("test.v") != nil
}

// ConfigDir is the global config layer: ~/.config/stavlos.
func ConfigDir() string {
	if v := os.Getenv("STAVLOS_CONFIG_DIR"); v != "" {
		return v
	}
	return filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "stavlos")
}

// DataDir holds the event log, trust records, and the socket.
func DataDir() string {
	if v := os.Getenv("STAVLOS_DATA_DIR"); v != "" {
		return v
	}
	return filepath.Join(xdg("XDG_DATA_HOME", ".local/share"), "stavlos")
}

// CacheDir holds models.dev and plugin caches.
func CacheDir() string {
	if v := os.Getenv("STAVLOS_CACHE_DIR"); v != "" {
		return v
	}
	return filepath.Join(xdg("XDG_CACHE_HOME", ".cache"), "stavlos")
}

// Socket is the daemon's Unix socket path. It prefers $XDG_RUNTIME_DIR
// (short, per-user, tmpfs) because Unix socket paths are limited to ~100
// bytes; it falls back to the data directory.
func Socket() string {
	if v := os.Getenv("STAVLOS_SOCKET"); v != "" {
		return v
	}
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" && !underTest() {
		return filepath.Join(rt, "stavlosd.sock")
	}
	return filepath.Join(DataDir(), "stavlosd.sock")
}

// AuthFile is the credential store (mode 0600).
func AuthFile() string { return filepath.Join(DataDir(), "auth.json") }

// ProjectDir is the project config layer inside a working directory.
func ProjectDir(workdir string) string { return filepath.Join(workdir, ".stavlos") }
