// Package clip shortens text for a model or a screen without cutting a
// character in half: the one head-and-tail rule every tool result, job
// result and summary uses.
package clip

import (
	"fmt"
	"unicode/utf8"
)

// DefaultMax is the size text is clipped to when no limit is given.
const DefaultMax = 32 * 1024

// Middle keeps the first two thirds and the last third of s within max
// bytes (0 means DefaultMax), with a note of how much was dropped between.
func Middle(s string, max int) string {
	if max <= 0 {
		max = DefaultMax
	}
	if len(s) <= max {
		return s
	}
	head := max * 2 / 3
	return Head(s, head) + fmt.Sprintf("\n\n… [%d bytes truncated] …\n\n", len(s)-max) + Tail(s, max-head)
}

// Head is at most the first n bytes of s, cut on a rune boundary.
func Head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Tail is at most the last n bytes of s, cut on a rune boundary.
func Tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}
