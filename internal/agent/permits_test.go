package agent

import (
	"testing"

	"github.com/nicodes/stavlos/internal/policy"
)

func TestPrefixFor(t *testing.T) {
	if prefixFor(policy.KindURL, "https://Docs.Go.dev/pkg/x?y") != "docs.go.dev" || prefixFor(policy.KindURL, "pkg.go.dev/net") != "pkg.go.dev" || prefixFor(policy.KindPath, "/x") != "" || prefixFor(policy.KindCommand, "go test ./x") != "go test" || prefixFor(policy.KindCommand, "go test; rm x") != "" || prefixFor(policy.KindText, "x") != "" || prefixFor(policy.KindID, "a1") != "" {
		t.Fatal("prefixFor")
	}
	if !prefixCovers(policy.KindURL, "docs.go.dev", "https://docs.go.dev/other") || prefixCovers(policy.KindURL, "docs.go.dev", "https://evil.docs.go.dev/") || prefixCovers(policy.KindURL, "", "https://x/") || !prefixCovers(policy.KindCommand, "go test", "go test ./...") || prefixCovers(policy.KindCommand, "go test", "go test; rm x") || prefixCovers(policy.KindPath, "/", "/x") {
		t.Fatal("prefixCovers")
	}
	var p permits
	p.rememberPrefix("shell", "go test")
	p.rememberPrefix("shell", "go test") // once
	p.rememberCall("read", "/etc/hosts")
	if len(p.prefixes["shell"]) != 1 || !p.covers("shell", policy.Command("go test ./...")) || p.covers("shell", policy.Command("go build")) || !p.covers("read", policy.Path("/etc/hosts")) || p.covers("read", policy.Path("/etc/passwd")) {
		t.Fatal("permits")
	}
	// A patch is covered only when every path it touches is.
	p.rememberCall("apply_patch", "a.go")
	if !p.covers("apply_patch", policy.Path("a.go")) || p.covers("apply_patch", policy.Path("a.go", "../../.bashrc")) || p.covers("apply_patch", policy.Path()) {
		t.Fatal("a permit for one path covered another")
	}
}
