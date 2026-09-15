package transcript

// Streaming reports whether a live buffer is being shown.
func (t *Transcript) Streaming() bool { return len(t.stream) > 0 }

// ItemRange returns the first and last index into All() of item i, or
// (-1, -1) when there is no such item.
func (t *Transcript) ItemRange(i int) (first, last int) { return ItemRange(t.All(), i) }
