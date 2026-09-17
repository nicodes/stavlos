package main

import (
	"github.com/nicodes/stavlos/internal/protocol"
	"testing"
)

func TestResumeAcrossWorkingDirectories(t *testing.T) {
	channels := []protocol.ChannelInfo{
		{ID: "archived", Archived: true, Dir: "/work/old"},
		{ID: "newest", Dir: "/work/site"},
		{ID: "viewed", Dir: "/work/backend"},
	}
	if got := resumeChannel(channels, "viewed"); got != "viewed" {
		t.Fatalf("got %q", got)
	}
	for _, missing := range []string{"", "removed", "archived"} {
		if got := resumeChannel(channels, missing); got != "newest" {
			t.Fatalf("fallback %q: %q", missing, got)
		}
	}
	if resumeChannel(nil, "viewed") != "" {
		t.Fatal("an empty catalog should create the first channel")
	}
}
