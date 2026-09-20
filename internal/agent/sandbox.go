package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/instructions"
	"github.com/nicodes/stavlos/internal/paths"
	"github.com/nicodes/stavlos/internal/pathx"
	"github.com/nicodes/stavlos/internal/policy"
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
// later (git hooks and config, .stavlos, .envrc, and every AGENTS.md or
// CLAUDE.md the trust hash knows) are read-only.

// sandboxReadOnly are the control paths, relative to each working
// directory, a command may read but not change. git's objects and refs
// stay writable: only what runs code or steers an agent is frozen.
var sandboxReadOnly = append([]string{".git/hooks", ".git/config", ".stavlos", ".envrc"}, instructions.Names...)

// sandboxHiddenHome are credential stores, relative to the home directory.
var sandboxHiddenHome = []string{
	".ssh", ".gnupg", ".aws", ".azure", ".kube", ".docker", ".netrc", ".git-credentials", ".pypirc", ".npmrc",
	".cargo/credentials.toml", ".config/gh", ".config/hub", ".config/gcloud", ".password-store", ".local/share/keyrings",
}

// sandboxSpec is the boundary for the channel's commands under cfg, nil
// when the sandbox is turned off.
func (c *Channel) sandboxSpec(cfg *config.Effective) *sandbox.Spec {
	if !cfg.Sandbox.Enabled {
		return nil
	}
	spec := &sandbox.Spec{Network: cfg.Sandbox.Network}
	for _, d := range c.dirPaths() {
		d = tools.ResolvePath("", d)
		spec.Writable = append(spec.Writable, d)
		for _, ro := range sandboxReadOnly {
			spec.ReadOnly = append(spec.ReadOnly, filepath.Join(d, ro))
		}
	}
	spec.ReadOnly = append(spec.ReadOnly, cfg.InstructionFiles...) // the nested ones too
	dirs := append([]string(nil), spec.Writable...)
	spec.Writable = append(spec.Writable, buildCaches()...)
	spec.Writable = append(spec.Writable, cfg.Sandbox.Writable...)
	// The channel's scratch directory is TMPDIR, and replaces /tmp unless a
	// working directory lives there (it would vanish under the mount).
	tmp := filepath.Join(paths.CacheDir(), "tmp", c.ID)
	if err := os.MkdirAll(tmp, 0o700); err == nil {
		spec.Tmp, spec.PrivateTmp = tmp, !anyWithin(dirs, os.TempDir())
	}
	spec.Hidden = hiddenPaths(cfg, dirs)
	return spec
}

// sandboxLevel is what bounds a channel's commands, as clients are told it.
func sandboxLevel(cfg *config.Effective) string {
	if !cfg.Sandbox.Enabled {
		return "off"
	}
	lvl, _ := probeSandbox()
	return lvl.String()
}

// probeSandbox is sandbox.Probe, a variable so a test can be a kernel that
// offers nothing.
var probeSandbox = sandbox.Probe

// unsandboxed reports that the sandbox is wanted and the kernel offers none
// of it: a command would run with nothing between it and the machine.
func unsandboxed(cfg *config.Effective) bool { return sandboxLevel(cfg) == "none" }

// hiddenPaths is what no agent may reach, whatever the mode: the harness's
// own data, configuration and socket, the user's runtime directory, and the
// credential stores in the home directory, with what stavlos.json adds. The
// sandbox mounts nothing over them for commands, and the file tools are
// refused them (permission.go): a list that bound only the shell was a list
// an agent could read its way round. A hidden path that holds a working
// directory is left out, since hiding it would take the directory along.
func hiddenPaths(cfg *config.Effective, dirs []string) []string {
	home, _ := os.UserHomeDir()
	hidden := []string{paths.ConfigDir(), paths.DataDir(), paths.CacheDir(), paths.Socket(), filepath.Join("/run/user", itoa(os.Getuid()))}
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		hidden = append(hidden, rt)
	}
	for _, h := range sandboxHiddenHome {
		hidden = append(hidden, filepath.Join(home, h))
	}
	var out []string
	for _, h := range append(hidden, cfg.Sandbox.Hide...) {
		if !anyWithin(dirs, h) {
			out = append(out, h)
		}
	}
	return out
}

// hiddenFrom is the hidden path a file tool's subject reaches into, or "".
// What the channel itself keeps under the harness's directories stays open
// to it: its sheets and its scratch directory.
func (c *Channel) hiddenFrom(sub policy.Subject, cfg *config.Effective) string {
	if sub.Kind != policy.KindPath {
		return ""
	}
	var dirs []string
	for _, d := range c.dirPaths() {
		dirs = append(dirs, tools.ResolvePath("", d))
	}
	own := []string{tools.ResolvePath("", c.SheetDir()), tools.ResolvePath("", filepath.Join(paths.CacheDir(), "tmp", c.ID))}
	for _, v := range sub.Values {
		p := tools.ResolvePath(c.Dir(), v)
		if slices.ContainsFunc(own, func(o string) bool { return pathx.Within(o, p) }) {
			continue
		}
		for _, h := range hiddenPaths(cfg, dirs) {
			if pathx.Within(tools.ResolvePath("", h), p) {
				return h
			}
		}
	}
	return ""
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
	for _, p := range paths {
		if pathx.Within(dir, p) {
			return true
		}
	}
	return false
}

func itoa(n int) string { return strconv.Itoa(n) }
