package agent

import (
	"os"
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
		{"cat README.md", nil},                                     // relative arguments stay in the working directory
		{"/usr/bin/env python3 run.py", nil},                       // the program is not a data path
		{"cat /etc/hostname", []string{"/etc/hostname"}},           // an absolute argument
		{"grep -r x ~/other", []string{home() + "/other"}},         // ~ expands
		{"cd /tmp/build && make", []string{"/tmp/build"}},          // cd target
		{"cd ../sibling; ls", []string{"/work/sibling"}},           // relative cd target, from the session directory
		{"go test ./... > /tmp/out.txt", []string{"/tmp/out.txt"}}, // redirect target
		{"make 2>/var/log/x.log", []string{"/var/log/x.log"}},      // attached redirect
		{"rg --path=/srv/data foo", []string{"/srv/data"}},         // --flag=path
		{"ls /opt/*/bin", []string{"/opt"}},                        // a glob is cut at its wildcard
		{"echo hi > /dev/null", nil},                               // /dev is never a boundary
		{"cat \"/etc/passwd\"", []string{"/etc/passwd"}},           // quotes are stripped
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
