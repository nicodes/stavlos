package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBashPathCandidates(t *testing.T) {
	base := "/work/repo"
	cases := []struct {
		cmd  string
		want []string
	}{
		{"ls", nil},
		{"cat README.md", nil},                                       // relative arguments stay in the working directory
		{"/usr/bin/env python3 run.py", nil},                         // the program is not a data path
		{"cat /etc/hostname", []string{"/etc/hostname"}},             // an absolute argument
		{"grep -r x ~/other", []string{home() + "/other"}},           // ~ expands
		{"cd /tmp/build && make", []string{"/tmp/build"}},            // cd target
		{"cd ../sibling; ls", []string{"/work/sibling"}},             // relative cd target, from the session directory
		{"go test ./... > /tmp/out.txt", []string{"/tmp/out.txt"}},   // redirect target
		{"make 2>/var/log/x.log", []string{"/var/log/x.log"}},        // attached redirect
		{"rg --path=/srv/data foo", []string{"/srv/data"}},           // --flag=path
		{"ls /opt/*/bin", []string{"/opt"}},                          // a glob is cut at its wildcard
		{"echo hi > /dev/null", nil},                                 // /dev is never a boundary
		{"cat \"/etc/passwd\"", []string{"/etc/passwd"}},             // quotes are stripped
		{"cat ../../etc/passwd", []string{"/etc/passwd"}},            // parent-relative arguments leave the working directory
		{"cp ../x .", []string{"/work/x"}},                           // one level up
		{"cat ./a/../b", []string{"/work/repo/b"}},                   // a .. inside stays inside (the caller checks the set)
		{"cat $HOME/.ssh/id_rsa", []string{home() + "/.ssh/id_rsa"}}, // $HOME expands
		{"ls ${PWD}/sub", []string{"/work/repo/sub"}},                // $PWD is the working directory
		{"cat $UNKNOWN/x", nil},                                      // an unknown variable is not a path we can judge
	}
	for _, c := range cases {
		if got := bashPathCandidates(c.cmd, base); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %v want %v", c.cmd, got, c.want)
		}
	}
}

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

func TestGrantDir(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	os.MkdirAll(filepath.Join(repo, "internal", "agent"), 0o755)
	os.WriteFile(filepath.Join(repo, "internal", "agent", "turn.go"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(root, "plain", "sub"), 0o755)
	os.WriteFile(filepath.Join(root, "plain", "sub", "f.txt"), []byte("x"), 0o644)
	cases := map[string]string{
		filepath.Join(repo, "internal", "agent", "turn.go"): repo,                                    // a file inside a checkout: the checkout
		filepath.Join(repo, "internal"):                     repo,                                    // a directory inside it too
		filepath.Join(root, "plain", "sub", "f.txt"):        filepath.Join(root, "plain", "sub"),     // no checkout: the file's directory
		filepath.Join(root, "plain", "sub"):                 filepath.Join(root, "plain", "sub"),     // a directory: itself
		filepath.Join(root, "plain", "missing", "g.txt"):    filepath.Join(root, "plain", "missing"), // a path that does not exist yet: its parent
	}
	for p, want := range cases {
		if got := grantDir(p); got != want {
			t.Errorf("%s: got %s want %s", p, got, want)
		}
	}
}
