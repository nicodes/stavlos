package agent

import (
	"fmt"
	"path/filepath"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/paths"
	"github.com/nicodes/stavlos/internal/sandbox"
	"github.com/nicodes/stavlos/internal/tools"
)

func (c *Channel) agentDirsLocked(id string) []string {
	st := c.st.agents[id]
	if st == nil || st.directories == nil {
		return c.dirPathsLocked()
	}
	var out []string
	for _, dir := range st.directories {
		if inDirs(c.dirPathsLocked(), dir) {
			out = append(out, dir)
		}
	}
	return out
}
func (c *Channel) childScopeLocked(parent string, want []string) ([]string, error) {
	if parent == "" {
		return nil, nil
	}
	permitted := c.agentDirsLocked(parent)
	if len(permitted) == 0 {
		return nil, fmt.Errorf("parent has no task directories")
	}
	if len(want) == 0 {
		want = permitted[:1]
	}
	var out []string
	for _, dir := range want {
		dir = tools.ResolvePath(permitted[0], dir)
		if !inDirs(permitted, dir) {
			return nil, fmt.Errorf("child task directory is outside parent scope: %s", dir)
		}
		out = append(out, dir)
	}
	return out, nil
}
func (a *Agent) dirPaths() []string {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return a.c.agentDirsLocked(a.ID)
}
func (a *Agent) Dir() string {
	dirs := a.dirPaths()
	if len(dirs) > 0 {
		return dirs[0]
	}
	return a.c.Dir()
}
func (a *Agent) roleSandboxSpec(cfg *config.Effective, readonly bool) *sandbox.Spec {
	return a.c.roleSandboxSpecDirs(cfg, readonly, a.dirPaths())
}
func (a *Agent) fileRoots() []string {
	out := a.dirPaths()
	return append(out, tools.ResolvePath("", a.c.SheetDir()), tools.ResolvePath("", a.c.ScratchDir()))
}

// Keep process scratch separate from the hidden harness cache. On worktrees
// under /tmp a private /tmp mount cannot be used without hiding the worktree.
func (c *Channel) ScratchDir() string {
	return filepath.Join(filepath.Dir(paths.CacheDir()), "stavlos-tasks", c.ID)
}
