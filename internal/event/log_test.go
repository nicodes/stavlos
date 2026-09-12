package event

import (
	"context"
	"path/filepath"
	"testing"
)

func TestAppendSeqContiguousPerSession(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx := context.Background()
	ch, cancel := l.Subscribe("a")
	defer cancel()
	for i := 0; i < 3; i++ {
		if _, err := l.Append(ctx, Event{Session: "a", Type: PromptQueued, Payload: MustPayload(TextPayload{Text: "x"})}); err != nil {
			t.Fatal(err)
		}
		if _, err := l.Append(ctx, Event{Session: "b", Type: PromptQueued}); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := l.Read(ctx, "a", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Fatalf("want 3 got %d", len(evs))
	}
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("seq %d != %d", e.Seq, i+1)
		}
		if e.Global != int64(2*i+1) {
			t.Fatalf("global %d != %d", e.Global, 2*i+1)
		}
	}
	got := 0
	for len(ch) > 0 {
		e := <-ch
		if e.Session != "a" {
			t.Fatal("wrong session fanout")
		}
		got++
	}
	if got != 3 {
		t.Fatalf("fanout %d", got)
	}
	var tp TextPayload
	if err := evs[0].Decode(&tp); err != nil || tp.Text != "x" {
		t.Fatal("payload roundtrip")
	}
}
