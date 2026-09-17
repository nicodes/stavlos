package transcript

import (
	"github.com/nicodes/stavlos/internal/toolname"
	"strings"
)

type explicitWait struct {
	ref      lineRef
	left     int
	names    []string
	answered map[string]bool
}
type chatRequest struct {
	names  map[string]bool
	legacy bool
}

func messageRequestID(output string) string {
	_, rest, ok := strings.Cut(output, "; request_id: ")
	if !ok {
		return ""
	}
	if fields := strings.Fields(rest); len(fields) > 0 {
		return fields[0]
	}
	return ""
}

func (t *Transcript) trackExplicit(id string, ref lineRef, names []string) {
	if t.explicitAsks == nil {
		t.explicitAsks = map[string]*explicitWait{}
	}
	w := &explicitWait{ref: ref, names: names, answered: map[string]bool{}}
	for _, name := range names {
		if name != toolname.User {
			w.left++
		}
	}
	if w.left > 0 {
		t.explicitAsks[id] = w
		t.line(ref).Tone = ToneWorking
	}
}

func (t *Transcript) answeredRequests(from string, ids []string) {
	for _, id := range ids {
		w := t.explicitAsks[id]
		if w == nil || w.answered[from] {
			continue
		}
		w.answered[from] = true
		w.left--
		if w.left <= 0 {
			if l := t.line(w.ref); l != nil && l.Tone != ToneError {
				l.Tone = ToneNone
			}
			delete(t.explicitAsks, id)
		}
	}
}

func (t *Transcript) trackPost(id string, names []string, legacy bool) {
	if t.chatRequests == nil {
		t.chatRequests = map[string]*chatRequest{}
	}
	w := &chatRequest{names: map[string]bool{}, legacy: legacy}
	for _, name := range names {
		w.names[name] = true
	}
	t.chatRequests[id] = w
}

func (t *Transcript) settlePost(id, from string) {
	if r := t.chatRequests[id]; r != nil {
		delete(r.names, from)
		if len(r.names) == 0 {
			delete(t.chatRequests, id)
		}
	}
}
