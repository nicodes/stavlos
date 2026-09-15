package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/paths"
	"github.com/nicodes/stavlos/internal/sandbox"
	"github.com/nicodes/stavlos/internal/tools"
)

// The channel's sandbox: what its shell commands and MCP servers may
// change, what they may not see, and what stays read-only (package
// sandbox enforces it). Commands write beneath the working directories, a
// scratch directory of the channel (TMPDIR, mounted as /tmp), and the caches build
// tools fill; they cannot see the harness's data, config, cache or socket,
// the user's runtime directory (the D-Bus and agent sockets live there) or
// credential stores; and the files that steer the harness or run code
// later (git hooks and config, .stavlos, AGENTS.md, .envrc) are read-only.

// sandboxReadOnly are the control paths, relative to each working
// directory, a command may read but not change. git's objects and refs
// stay writable: only what runs code or steers an agent is frozen.
var sandboxReadOnly = []string{".git/hooks", ".git/config", ".stavlos", "AGENTS.md", ".envrc"}

// sandboxHiddenHome are credential stores, relative to the home directory.
var sandboxHiddenHome = []string{
	".ssh", ".gnupg", ".aws", ".azure", ".kube", ".docker", ".netrc", ".git-credentials", ".pypirc", ".npmrc",
	".cargo/credentials.toml", ".config/gh", ".config/hub", ".config/gcloud", ".password-store", ".local/share/keyrings",
}

// sandboxSpec is the boundary for the channel's commands under cfg, nil
// when the sandbox is turned off.
func (s *Channel) sandboxSpec(cfg *config.Effective) *sandbox.Spec {
	if !cfg.Sandbox.Enabled {
		return nil
	}
	spec := &sandbox.Spec{Network: cfg.Sandbox.Network}
	for _, d := range s.dirPaths() {
		d = tools.ResolvePath("", d)
		spec.Writable = append(spec.Writable, d)
		for _, ro := range sandboxReadOnly {
			spec.ReadOnly = append(spec.ReadOnly, filepath.Join(d, ro))
		}
	}
	dirs := append([]string(nil), spec.Writable...)
	spec.Writable = append(spec.Writable, buildCaches()...)
	spec.Writable = append(spec.Writable, cfg.Sandbox.Writable...)
	// The channel's scratch directory is TMPDIR, and replaces /tmp unless a
	// working directory lives there (it would vanish under the mount).
	tmp := filepath.Join(paths.CacheDir(), "tmp", s.ID)
	if err := os.MkdirAll(tmp, 0o700); err == nil {
		spec.Tmp, spec.PrivateTmp = tmp, !anyWithin(dirs, os.TempDir())
	}
	home, _ := os.UserHomeDir()
	hidden := []string{paths.ConfigDir(), paths.DataDir(), paths.CacheDir(), paths.Socket(), filepath.Join("/run/user", itoa(os.Getuid()))}
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		hidden = append(hidden, rt)
	}
	for _, h := range sandboxHiddenHome {
		hidden = append(hidden, filepath.Join(home, h))
	}
	for _, h := range append(hidden, cfg.Sandbox.Hide...) {
		// A hidden path that holds a working directory would take it along.
		if !anyWithin(dirs, h) {
			spec.Hidden = append(spec.Hidden, h)
		}
	}
	return spec
}

// buildCaches are the per-user directories build tools write as they work,
// where they exist. Tool install directories (~/go/bin, ~/.cargo/bin) are
// not among them: a command must not plant a program the user runs later.
func buildCaches() []string {
	home, _ := os.UserHomeDir()
	cache, _ := os.UserCacheDir()
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		gopath = filepath.Join(home, "go")
	}
	var out []string
	for _, p := range []string{
		cache, os.Getenv("GOCACHE"), os.Getenv("GOMODCACHE"), os.Getenv("GOTMPDIR"), filepath.Join(gopath, "pkg", "mod"),
		filepath.Join(home, ".npm"), filepath.Join(home, ".cargo", "registry"), filepath.Join(home, ".cargo", "git"),
		filepath.Join(home, ".m2", "repository"), filepath.Join(home, ".gradle", "caches"),
		filepath.Join(home, ".local", "share", "pnpm", "store"), filepath.Join(home, ".bun", "install", "cache"),
	} {
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

// anyWithin reports whether any of paths is dir or lies beneath it.
func anyWithin(paths []string, dir string) bool {
	dir = filepath.Clean(dir)
	for _, p := range paths {
		if p == dir || strings.HasPrefix(p, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func itoa(n int) string { return strconv.Itoa(n) }
