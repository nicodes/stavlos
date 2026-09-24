package toolname

import (
	"slices"
	"testing"
)

// TestExpandKeepsOrderAndDropsRepeats: a name comes out once, where it was
// first seen, and nil in gives nil out.
func TestExpandKeepsOrderAndDropsRepeats(t *testing.T) {
	got := Expand([]string{Shell, Read, Shell, Grep, Read, Todo})
	if want := []string{Shell, Read, Grep, Todo}; !slices.Equal(got, want) {
		t.Errorf("Expand: %v, want %v", got, want)
	}
	if got := Expand(nil); got != nil {
		t.Errorf("Expand(nil): %v", got)
	}
	if got := Expand([]string{Sheet}); !slices.Equal(got, []string{Sheet}) {
		t.Errorf("one name is itself: %v", got)
	}
}
