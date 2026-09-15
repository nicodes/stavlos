// Package paths centralises filesystem locations (PRD §10.1, §10.7, §11.3).
package paths

import (
	"os"
	"path/filepath"
)

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

func xdg(env, fallback string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return filepath.Join(home(), fallback)
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
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		return filepath.Join(rt, "stavlosd.sock")
	}
	return filepath.Join(DataDir(), "stavlosd.sock")
}

// AuthFile is the credential store (mode 0600).
func AuthFile() string { return filepath.Join(DataDir(), "auth.json") }

// ProjectDir is the project config layer inside a working directory.
func ProjectDir(workdir string) string { return filepath.Join(workdir, ".stavlos") }
