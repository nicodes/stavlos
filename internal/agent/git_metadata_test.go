package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/sandbox"
)

func TestExplicitWorktreeMetadataSupportsGitAndFreezesControls(t *testing.T) {
	base, err := os.MkdirTemp("/var/tmp", "stavlos-git-grant-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	common, work, other := filepath.Join(base, "origin.git"), filepath.Join(base, "work"), filepath.Join(base, "other")
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	git("init", "--bare", common)
	c := exec.Command("git", "--git-dir="+common, "hash-object", "-t", "tree", "--stdin")
	c.Stdin = strings.NewReader("")
	tree, err := c.Output()
	if err != nil {
		t.Fatal(err)
	}
	c = exec.Command("git", "--git-dir="+common, "-c", "user.name=Fixture", "-c", "user.email=fixture@localhost", "commit-tree", strings.TrimSpace(string(tree)))
	c.Stdin = strings.NewReader("seed\n")
	commit, err := c.Output()
	if err != nil {
		t.Fatal(err)
	}
	git("--git-dir="+common, "update-ref", "refs/heads/main", strings.TrimSpace(string(commit)))
	git("--git-dir="+common, "worktree", "add", "-b", "candidate", work, "main")
	git("--git-dir="+common, "worktree", "add", "-b", "other", other, "main")
	own := filepath.Join(common, "worktrees", "work")
	for _, dir := range []string{own, filepath.Join(common, "worktrees", "other")} {
		if err := os.WriteFile(filepath.Join(dir, "config.worktree"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(common, "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	spec := sandbox.Spec{Writable: []string{work}, ReadOnly: []string{filepath.Join(work, ".git")}}
	grantGitMetadata(&spec, []string{work}, nil)
	if slices.Contains(spec.Readable, common) {
		t.Fatal("untrusted .git pointer granted authority")
	}
	grantGitMetadata(&spec, []string{work}, []string{common})
	if !slices.Contains(spec.Writable, own) || slices.Contains(spec.Writable, common) || slices.Contains(spec.Writable, other) {
		t.Fatalf("unexpected Git grant: %+v", spec)
	}
	if level, _ := sandbox.Probe(); level != sandbox.Full {
		t.Skip("full sandbox required for actual Git checks")
	}
	run := func(script string) (string, error) {
		c := exec.Command("sh", "-c", script)
		c.Dir = work
		c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if _, err := sandbox.Wrap(c, spec); err != nil {
			t.Fatal(err)
		}
		out, err := c.CombinedOutput()
		return string(out), err
	}
	if out, err := run("set -e; printf fixed > result; git add result; git -c user.name=Fixture -c user.email=fixture@localhost commit -m fixed; git status --porcelain"); err != nil || !strings.Contains(out, "fixed") {
		t.Fatalf("normal Git: %v %s", err, out)
	}
	for _, path := range []string{filepath.Join(work, ".git"), filepath.Join(common, "config"), filepath.Join(common, "hooks", "pre-commit.sample"), filepath.Join(own, "config.worktree"), filepath.Join(own, "commondir"), filepath.Join(common, "worktrees", "other", "HEAD")} {
		if out, err := run("printf changed > '" + path + "'"); err == nil {
			t.Fatalf("protected path mutated: %s %s", path, out)
		}
	}
	// Read-only roles retain Git inspection but cannot mutate metadata.
	channel, _ := newTestChannel(t, testConfig{}, &fakeModel{})
	cfg := *channel.Config()
	cfg.Sandbox.Enabled = true
	cfg.Sandbox.GitMetadata = []string{common}
	readonly := channel.roleSandboxSpecDirs(&cfg, true, []string{work})
	for _, path := range readonly.Writable {
		if strings.HasPrefix(path, common+string(filepath.Separator)) {
			t.Fatal("read-only role retained writable Git metadata")
		}
	}
	frozen := *readonly
	previous := spec
	spec = frozen
	if out, err := run("git status --porcelain"); err != nil {
		t.Fatalf("read-only Git inspection: %v %s", err, out)
	}
	if out, err := run("git update-ref refs/heads/candidate HEAD~0"); err == nil {
		t.Fatalf("read-only Git mutated refs: %s", out)
	}
	spec = previous
	// Missing controls must fail closed rather than be creatable by commands.
	if err := os.Remove(filepath.Join(own, "config.worktree")); err != nil {
		t.Fatal(err)
	}
	absent := sandbox.Spec{}
	grantGitMetadata(&absent, []string{work}, []string{common})
	if len(absent.Writable) != 0 {
		t.Fatal("missing worktree config received write access")
	}
	// Repointing metadata at an adjacent directory does not grant it.
	if err := os.WriteFile(filepath.Join(work, ".git"), []byte("gitdir: "+filepath.Join(base, "foreign")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	denied := sandbox.Spec{}
	grantGitMetadata(&denied, []string{work}, []string{common})
	if len(denied.Readable) != 0 || len(denied.Writable) != 0 {
		t.Fatal("foreign metadata accepted")
	}
}
