package protocol

import "testing"

func TestCommandPrefix(t *testing.T) {
	cases := map[string]string{
		"go test ./... -run TestRoles": "go test",
		"go -C x test":                 "go",
		"ls -la":                       "ls",
		"git status":                   "git status",
		"npm run build":                "npm run",
		"echo one; rm -rf /":           "",
		"go test && echo ok":           "",
		"cat $(ls)":                    "",
		"a | b":                        "",
		"":                             "",
		"  make   build  ":             "make build",
	}
	for cmd, want := range cases {
		if got := CommandPrefix(cmd); got != want {
			t.Errorf("CommandPrefix(%q) = %q, want %q", cmd, got, want)
		}
	}
	if !PrefixCovers("go test", "go test ./...") || !PrefixCovers("go test", "go test") || PrefixCovers("go test", "go testx") || PrefixCovers("go test", "go test; rm x") || PrefixCovers("", "go test") {
		t.Fatal("PrefixCovers")
	}
}
