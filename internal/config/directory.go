package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// WorkingDirectory resolves a directory entered by the human against base.
// It never falls back to the daemon's process working directory.
func WorkingDirectory(base, dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("a default directory is required")
	}
	dir = expandPath(dir)
	if !filepath.IsAbs(dir) {
		if !filepath.IsAbs(base) {
			return "", errors.New("an absolute default directory is required")
		}
		dir = filepath.Join(base, dir)
	}
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	if msg := DirectoryError(dir); msg != "" {
		return "", errors.New(msg)
	}
	return filepath.Clean(dir), nil
}

// DirectoryError describes an unavailable working directory without hiding
// the channel or changing where it works.
func DirectoryError(dir string) string {
	st, err := os.Stat(dir)
	if err != nil {
		return "default directory unavailable: " + err.Error()
	}
	if !st.IsDir() {
		return "default directory is not a directory: " + dir
	}
	return ""
}
