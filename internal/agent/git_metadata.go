package agent

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/nicodes/stavlos/internal/sandbox"
)

// A repository-controlled .git pointer never establishes authority. The common
// directory must be explicitly granted and the assigned worktree must identify
// its own metadata beneath that directory. Other worktrees remain read-only.
func grantGitMetadata(spec *sandbox.Spec, scope, grants []string) {
	for _, work := range scope {
		link := filepath.Join(work, ".git")
		body, ok := smallGitFile(link)
		if !ok || !strings.HasPrefix(body, "gitdir: ") {
			continue
		}
		own := filepath.Clean(strings.TrimSpace(strings.TrimPrefix(body, "gitdir: ")))
		if !filepath.IsAbs(own) {
			own = filepath.Join(work, own)
		}
		commonBody, ok := smallGitFile(filepath.Join(own, "commondir"))
		if !ok {
			continue
		}
		common := strings.TrimSpace(commonBody)
		if !filepath.IsAbs(common) {
			common = filepath.Join(own, common)
		}
		common = filepath.Clean(common)
		allowed := false
		for _, grant := range grants {
			real, err := filepath.EvalSymlinks(grant)
			if err == nil && filepath.IsAbs(grant) && filepath.Clean(grant) == real && real == common {
				allowed = true
			}
		}
		if !allowed || filepath.Dir(own) != filepath.Join(common, "worktrees") {
			continue
		}
		real, err := filepath.EvalSymlinks(own)
		if err != nil || real != own {
			continue
		}
		back, ok := smallGitFile(filepath.Join(own, "gitdir"))
		if !ok || filepath.Clean(strings.TrimSpace(back)) != link {
			continue
		}
		// Existing controls can be mounted read-only. An absent per-worktree
		// config would be creatable beneath its writable directory; require
		// the operator to prepare it before granting this metadata.
		if _, ok := smallGitFile(filepath.Join(own, "config.worktree")); !ok {
			continue
		}
		if _, ok := smallGitFile(filepath.Join(common, "config")); !ok {
			continue
		}
		hooks := filepath.Join(common, "hooks")
		if real, err := filepath.EvalSymlinks(hooks); err != nil || real != hooks {
			continue
		}
		addGitMetadata(spec, link, common, own)
	}
}

func smallGitFile(path string) (string, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return "", false
	}
	body, err := os.ReadFile(path)
	return string(body), err == nil
}

func addGitMetadata(spec *sandbox.Spec, link, common, own string) {
	spec.Readable = append(spec.Readable, common)
	for _, path := range []string{own, filepath.Join(common, "objects"), filepath.Join(common, "refs"), filepath.Join(common, "logs")} {
		real, err := filepath.EvalSymlinks(path)
		if err == nil && real == path {
			spec.Writable = append(spec.Writable, path)
		}
	}
	spec.ReadOnly = append(spec.ReadOnly, link, filepath.Join(common, "config"), filepath.Join(common, "hooks"), filepath.Join(own, "config.worktree"), filepath.Join(own, "commondir"), filepath.Join(own, "gitdir"))
}
