package agent

import (
	"crypto/sha256"
	"fmt"

	"github.com/nicodes/stavlos/internal/model"
)

// A file read again is answered from the context while the first result is
// still there. In one channel of 5,117 reads, 1,459 (29%) re-read a path the
// same agent had already read (docs/token-efficiency.md, finding 4); the
// bytes were already in the context, so a second copy taught the model
// nothing and cost a full result on every later call. The check is on the
// content, not the file's time, so an edit in between is a full read again;
// and the note is short-lived: once ClearOld takes the first result out of
// the projection (forgetReads), or a compaction summarises it, the next
// read is a full one.

// readSeen is what a read returned and which call carries it.
type readSeen struct {
	hash   [sha256.Size]byte
	callID string
}

// dedupRead answers a read whose result the context still holds with a note
// naming the call that holds it, and records any other.
func (a *Agent) dedupRead(c model.Block, out string) string {
	key := string(c.Input)
	h := sha256.Sum256([]byte(out))
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	if seen, ok := a.reads[key]; ok && seen.hash == h {
		return fmt.Sprintf("[unchanged since your read %s: its content is still in your context]", seen.callID)
	}
	if a.reads == nil {
		a.reads = map[string]readSeen{}
	}
	a.reads[key] = readSeen{hash: h, callID: c.ID}
	return out
}

// forgetReads drops the reads whose results ClearOld took out of the
// projection: the content is no longer in the context, so the next read of
// the file is a full one. Called with c.mu held.
func (a *Agent) forgetReads(cleared []string) {
	if len(cleared) == 0 || len(a.reads) == 0 {
		return
	}
	gone := make(map[string]bool, len(cleared))
	for _, id := range cleared {
		gone[id] = true
	}
	for key, seen := range a.reads {
		if gone[seen.callID] {
			delete(a.reads, key)
		}
	}
}
