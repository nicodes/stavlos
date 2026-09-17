package discord

import (
	"context"
	"testing"

	"github.com/nicodes/stavlos/internal/event"
)

func TestJoinAnnouncementsUseTheCreatingAgentsIdentity(t *testing.T) {
	_, w, api := fixture(t, nil)
	ctx := context.Background()
	spawn := func(seq int64, id, parent, name, role string) {
		t.Helper()
		if err := w.mirror(ctx, event.Event{Seq: seq, Channel: w.id, Agent: id, Type: event.AgentSpawned, Payload: event.MustPayload(event.AgentSpawnedPayload{ID: id, Parent: parent, Name: name, Role: role})}); err != nil {
			t.Fatal(err)
		}
	}
	spawn(1, "root", "", "main", "general")
	spawn(2, "child", "root", "scout", "explorer")
	spawn(3, "grandchild", "child", "reader", "general") // before the next tree poll
	name := "researcher"
	if err := w.mirror(ctx, event.Event{Seq: 4, Channel: w.id, Agent: "child", Type: event.AgentUpdated, Payload: event.MustPayload(event.AgentUpdatedPayload{Name: &name})}); err != nil {
		t.Fatal(err)
	}
	spawn(5, "reviewer", "child", "reviewer", "reviewer")
	spawn(2, "child", "root", "scout", "explorer") // replay must not re-announce or undo the rename
	want := map[string]string{
		"main joined (general).":      "",
		"scout joined (explorer).":    "main",
		"reader joined (general).":    "scout",
		"reviewer joined (reviewer).": "researcher",
	}
	for _, m := range api.snapshot() {
		if author, ok := want[m.Text]; ok {
			if m.User != author {
				t.Fatalf("%q authored by %q, want %q", m.Text, m.User, author)
			}
			delete(want, m.Text)
		}
	}
	if len(want) != 0 || len(api.snapshot()) != 5 || w.agentName("child") != "researcher" {
		t.Fatalf("missing/duplicate announcements or stale identity: %v", want)
	}
}
