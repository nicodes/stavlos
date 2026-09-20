package shellcmd

import (
	"strings"
	"testing"
)

// FuzzCommands: whatever a model writes as a command line, the analysis
// terminates without panicking, and its answers agree with each other: a
// prefix is offered only for a simple line, and covers the line it was
// offered for; a prefix never covers a line that is not simple, since an allow
// for "go test" must say nothing about "go test; rm -rf ~".
func FuzzCommands(f *testing.F) {
	for _, s := range []string{
		"go test ./...", "cat x; rm -rf ~", "echo $(whoami)", "a=rm; $a -rf /", "if true; then rm -rf /; fi",
		"sudo -u root rm -rf /", "bash -c 'echo hi'", "cat <<< x", "echo 'unterminated", "env -u X ls", "! ls", "",
		"git commit -m \"a; b\"", "ls | wc -l", "ls > out", "timeout 5 go build", "\\", "`", "$((",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		cmds := Commands(line)
		simple := Simple(line)
		_, _ = Words(line)
		prefix := Prefix(line)
		// (a simple line may still list more than one command: "bash -c 'x'"
		// lists the script inside it too, so that a deny rule sees it)
		_ = cmds
		if prefix != "" {
			if !simple {
				t.Fatalf("a prefix %q is offered for %q, which is not a simple command", prefix, line)
			}
			if !Covers(prefix, line) {
				t.Fatalf("the prefix %q offered for %q does not cover it", prefix, line)
			}
		}
		if !simple && strings.TrimSpace(line) != "" {
			for _, p := range []string{"go test", "git", "ls", "cat"} {
				if Covers(p, line) {
					t.Fatalf("prefix %q covers %q, which is not a simple command", p, line)
				}
			}
		}
	})
}
