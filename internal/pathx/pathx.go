// Package pathx answers the one question every boundary in the harness
// asks: is this path inside that directory. It was written out seven times
// (the working set, the sandbox's list, control files, sheets, nested
// instructions, a command's arguments, search results), as string prefixes
// and as filepath.Rel, which disagree at the edges: a prefix test on "/"
// asks for "//", and a Rel test for ".." also catches a file named "..x".
package pathx

import (
	"path/filepath"
	"strings"
)

// Rel is p relative to dir when p is dir or lies beneath it. Both are
// compared as cleaned text: a caller that must not be fooled by a symlink
// resolves them first (tools.ResolvePath), and says so by doing it.
func Rel(dir, p string) (rel string, ok bool) {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// Within reports whether p is dir or lies beneath it.
func Within(dir, p string) bool {
	_, ok := Rel(dir, p)
	return ok
}

// Under reports whether p lies beneath dir and is not dir itself.
func Under(dir, p string) bool {
	rel, ok := Rel(dir, p)
	return ok && rel != "."
}
