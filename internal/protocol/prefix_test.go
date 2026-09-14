package protocol

import "testing"

func TestToolPrefix(t *testing.T) {
	if ToolPrefix("web_fetch", "https://Docs.Go.dev/pkg/x?y") != "docs.go.dev" || ToolPrefix("web_fetch", "pkg.go.dev/net") != "pkg.go.dev" || ToolPrefix("read", "/x") != "" || ToolPrefix("shell", "go test ./x") != "go test" || ToolPrefix("shell", "go test; rm x") != "" {
		t.Fatalf("ToolPrefix: %q %q", ToolPrefix("web_fetch", "https://Docs.Go.dev/pkg/x?y"), ToolPrefix("web_fetch", "pkg.go.dev/net"))
	}
	if !ToolPrefixCovers("web_fetch", "docs.go.dev", "https://docs.go.dev/other") || ToolPrefixCovers("web_fetch", "docs.go.dev", "https://evil.docs.go.dev/") || ToolPrefixCovers("web_fetch", "", "https://x/") || !ToolPrefixCovers("shell", "go test", "go test ./...") || ToolPrefixCovers("shell", "go test", "go test; rm x") || ToolPrefixCovers("read", "/", "/x") {
		t.Fatal("ToolPrefixCovers")
	}
}
