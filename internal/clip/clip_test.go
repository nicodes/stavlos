package clip

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestHeadAndTailCutOnRuneBoundaries: a cut that would land inside a
// multi-byte character moves to the character's edge, and text that fits
// is returned whole.
func TestHeadAndTailCutOnRuneBoundaries(t *testing.T) {
	s := "héllo" // h(1) é(2) l l o
	for _, tc := range []struct {
		n          int
		head, tail string
	}{
		{0, "", ""},
		{1, "h", "o"},
		{2, "h", "lo"},   // Head: é needs bytes 1-2, so the cut moves back to 1; Tail: the last two bytes are "lo"
		{3, "hé", "llo"}, // Head: 3 bytes end exactly after é; Tail: bytes 2.. start mid-é, so the cut moves to 3
		{4, "hél", "llo"},
		{5, "héll", "éllo"},
		{6, "héllo", "héllo"},
		{9, "héllo", "héllo"},
	} {
		if got := Head(s, tc.n); got != tc.head || !utf8.ValidString(got) {
			t.Errorf("Head(%d) = %q, want %q", tc.n, got, tc.head)
		}
		if got := Tail(s, tc.n); got != tc.tail || !utf8.ValidString(got) {
			t.Errorf("Tail(%d) = %q, want %q", tc.n, got, tc.tail)
		}
	}
}

// TestMiddleKeepsHeadAndTailWithANote: the text is cut in the middle with a
// note saying how many bytes went; max<=0 means DefaultMax; the note counts
// the bytes over max, and a tiny max still yields valid, bounded output.
func TestMiddleKeepsHeadAndTailWithANote(t *testing.T) {
	short := strings.Repeat("x", 100)
	if got := Middle(short, 0); got != short {
		t.Errorf("under DefaultMax with max 0: %q", got)
	}
	head := DefaultMax * 2 / 3
	long := strings.Repeat("a", head) + strings.Repeat("x", 1000) + strings.Repeat("b", DefaultMax-head)
	for _, max := range []int{0, -1} {
		got := Middle(long, max)
		if !strings.Contains(got, "1000 bytes truncated") {
			t.Errorf("max %d: the note should count the bytes over DefaultMax: %q", max, got[head:head+80])
		}
		if !strings.HasPrefix(got, strings.Repeat("a", head)) || !strings.HasSuffix(got, strings.Repeat("b", DefaultMax-head)) || strings.Contains(got, "x") {
			t.Errorf("max %d: two thirds head, one third tail", max)
		}
	}
	s := "0123456789abcdefghij" // 20 bytes
	got := Middle(s, 12)
	if !strings.HasPrefix(got, "01234567\n\n") || !strings.HasSuffix(got, "\n\nghij") || !strings.Contains(got, fmt.Sprintf("… [%d bytes truncated", 8)) {
		t.Errorf("Middle(20 bytes, 12): %q", got)
	}
	// A tiny max: head and tail are within the limit and the output is only
	// the note longer than max.
	tiny := Middle(strings.Repeat("é", 50), 5)
	if !utf8.ValidString(tiny) {
		t.Errorf("a tiny max split a character: %q", tiny)
	}
	body := strings.ReplaceAll(tiny, "é", "")
	if len(tiny)-len(body) > 5 {
		t.Errorf("a tiny max kept %d bytes of text, over 5: %q", len(tiny)-len(body), tiny)
	}
}
