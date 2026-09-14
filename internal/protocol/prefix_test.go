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
	if ToolPrefix("web_fetch", "https://Docs.Go.dev/pkg/x?y") != "docs.go.dev" || ToolPrefix("web_fetch", "pkg.go.dev/net") != "pkg.go.dev" || ToolPrefix("read", "/x") != "" || ToolPrefix("shell", "go test ./x") != "go test" {
		t.Fatalf("ToolPrefix: %q %q", ToolPrefix("web_fetch", "https://Docs.Go.dev/pkg/x?y"), ToolPrefix("web_fetch", "pkg.go.dev/net"))
	}
	if !ToolPrefixCovers("web_fetch", "docs.go.dev", "https://docs.go.dev/other") || ToolPrefixCovers("web_fetch", "docs.go.dev", "https://evil.docs.go.dev/") || ToolPrefixCovers("web_fetch", "", "https://x/") || !ToolPrefixCovers("shell", "go test", "go test ./...") || ToolPrefixCovers("read", "/", "/x") {
		t.Fatal("ToolPrefixCovers")
	}
	if !PrefixCovers("go test", "go test ./...") || !PrefixCovers("go test", "go test") || PrefixCovers("go test", "go testx") || PrefixCovers("go test", "go test; rm x") || PrefixCovers("", "go test") {
		t.Fatal("PrefixCovers")
	}
}
