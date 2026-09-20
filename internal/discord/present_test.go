package discord

import (
	"testing"

	"github.com/nicodes/stavlos/internal/present"
)

// An approval card leads with the argument every other client shows for the
// tool: the table in present, not a second opinion held here.
func TestApprovalCardsLeadWithThePrimaryArgument(t *testing.T) {
	for tool, l := range permissionLayouts {
		if got, want := l.fields[0].key, present.PrimaryArg(tool); got != want {
			t.Errorf("%s leads with %q, present says %q", tool, got, want)
		}
	}
}
