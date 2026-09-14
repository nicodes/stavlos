package tui

import "github.com/nicodes/stavlos/internal/event"

// Test-only helpers on the transcript.

// Build folds a full event sequence into lines.
func Build(evs []event.Event) []Line {
	t := NewTranscript()
	for _, ev := range evs {
		t.Apply(ev)
	}
	return t.All()
}

// Streaming reports whether a live buffer is being shown.
func (t *Transcript) Streaming() bool { return len(t.stream) > 0 }

// ItemRange returns the first and last index into All() of item i, or
// (-1, -1) when there is no such item.
func (t *Transcript) ItemRange(i int) (first, last int) { return itemRange(t.All(), i) }

// Notice appends a local notice as one item.
func (t *Transcript) Notice(lines ...string) {
	ls := make([]Line, 0, len(lines))
	for i, l := range lines {
		ln := Line{Kind: LineNotice, Text: l}
		if i == 0 {
			ln.Glyph = GlyphNotice
		}
		ls = append(ls, ln)
	}
	t.appendItem(ls)
}
