package tui

import "testing"

func TestListWindow(t *testing.T) {
	for _, tc := range []struct {
		name               string
		cur, top, n, size  int
		wantStart, wantEnd int
	}{
		{"fits", 2, 0, 5, 8, 0, 5},
		{"stays put while the cursor is in view", 5, 3, 20, 4, 3, 7},
		{"follows the cursor down one row at a time", 7, 3, 20, 4, 4, 8},
		{"jumps up to the cursor", 1, 3, 20, 4, 1, 5},
		{"a stateless window pins the cursor to its bottom", 9, 0, 20, 8, 2, 10},
		{"ends at the last row", 19, 16, 20, 8, 16, 20},
		{"no rows", 0, 0, 0, 8, 0, 0},
	} {
		start, end := listWindow(tc.cur, tc.top, tc.n, tc.size)
		if start != tc.wantStart || end != tc.wantEnd {
			t.Errorf("%s: listWindow(%d, %d, %d, %d) = [%d, %d), want [%d, %d)", tc.name, tc.cur, tc.top, tc.n, tc.size, start, end, tc.wantStart, tc.wantEnd)
		}
	}
}
