package buildid

import (
	"strings"
	"testing"
)

func TestInfoString(t *testing.T) {
	got := Info{Version: "v0.1.0", Revision: "943f4acb07ef69d02a9590fb629bcde9eaadd832", Time: "2026-09-14T21:25:42Z", Modified: true,
		GoVersion: "go1.27.1", Platform: "linux/amd64", ID: "c3620f818139"}.String()
	want := "stavlos v0.1.0\ncommit  943f4acb07ef, 2026-09-14T21:25:42Z, with uncommitted changes\ngo      go1.27.1 linux/amd64\nbuild   c3620f818139\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	// Without version control information the commit line is left out.
	if s := (Info{Version: "devel", GoVersion: "go1.27.1", Platform: "linux/amd64", ID: "x"}).String(); strings.Contains(s, "commit") || !strings.HasPrefix(s, "stavlos devel\n") {
		t.Fatalf("no vcs:\n%s", s)
	}
}

func TestReadNeverEmpty(t *testing.T) {
	i := Read()
	if i.Version == "" || i.GoVersion == "" || i.Platform == "" || i.ID == "" {
		t.Fatalf("%+v", i)
	}
}
