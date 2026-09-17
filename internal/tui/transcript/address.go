package transcript

import (
	"github.com/nicodes/stavlos/internal/toolname"
	"strings"
)

// messageAddress omits the sender when the view supplies that perspective.
// Channel chat is the human's view, so its user recipient is also implicit.
func messageAddress(from string, to []string, viewer string) (string, []string) {
	var parts, names []string
	if from != "" && from != viewer {
		parts = append(parts, "@"+from+":")
		names = append(names, from)
	}
	for _, name := range to {
		if name == "" || viewer == toolname.User && name == toolname.User {
			continue
		}
		parts = append(parts, "@"+name)
		names = append(names, name)
	}
	return strings.Join(parts, " "), names
}
