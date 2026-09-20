package tools

import (
	"net/netip"
	"strings"
	"testing"
)

// FuzzParsePatch: a patch is text a model wrote. Parsing it never panics,
// and what it accepts names a file for every operation.
func FuzzParsePatch(f *testing.F) {
	for _, s := range []string{
		"*** Begin Patch\n*** Add File: a.txt\n+hi\n*** End Patch",
		"*** Begin Patch\n*** Update File: a.go\n@@ func main() {\n-old\n+new\n*** End Patch",
		"*** Begin Patch\n*** Delete File: a\n*** End Patch",
		"*** Begin Patch\n*** Update File: a\n*** Move to: b\n@@\n-x\n+y\n*** End Patch",
		"*** Begin Patch\n*** Update File: \n*** End Patch", "*** Begin Patch", "", "@@", "*** Add File: ../../etc/passwd\n+x",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		ops, err := parsePatch(text)
		if err != nil {
			return
		}
		for _, op := range ops {
			if strings.TrimSpace(op.path) == "" {
				t.Fatalf("an operation with no file was accepted: %+v from %q", op, text)
			}
		}
	})
}

// FuzzParseWebURL: whatever URL a model asks for, what is fetched is https,
// carries no credentials, and has a host.
func FuzzParseWebURL(f *testing.F) {
	for _, s := range []string{
		"https://example.com/x", "http://example.com", "example.com", "https://user:pw@example.com/", "file:///etc/passwd",
		"https://github.com/o/r/blob/main/a.go", "javascript:alert(1)", "https://[::1]/", "http://127.0.0.1:4999/ws", "//x", "", "https://",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		u, err := parseWebURL(raw)
		if err != nil {
			return
		}
		if u.Scheme != "https" || u.User != nil || u.Hostname() == "" {
			t.Fatalf("parseWebURL(%q) = %q: scheme %q, user %v, host %q", raw, u, u.Scheme, u.User, u.Hostname())
		}
	})
}

// FuzzPublicIP: an address the fetcher will dial is never loopback, private,
// link-local, multicast or unspecified, in either family or mapped.
func FuzzPublicIP(f *testing.F) {
	for _, s := range []string{"8.8.8.8", "127.0.0.1", "10.0.0.1", "::1", "::ffff:127.0.0.1", "169.254.169.254", "fe80::1", "fc00::1", "0.0.0.0", "224.0.0.1", "100.64.0.1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		ip, err := netip.ParseAddr(s)
		if err != nil || !publicIP(ip) {
			return
		}
		u := ip.Unmap()
		if u.IsLoopback() || u.IsPrivate() || u.IsLinkLocalUnicast() || u.IsLinkLocalMulticast() || u.IsMulticast() || u.IsUnspecified() || u.IsInterfaceLocalMulticast() {
			t.Fatalf("publicIP(%s) is true", s)
		}
	})
}
