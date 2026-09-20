package pathx

import "testing"

func TestWithin(t *testing.T) {
	for _, c := range []struct {
		dir, p string
		within bool
	}{
		{"/w", "/w", true},
		{"/w", "/w/a/b", true},
		{"/w/", "/w/a", true},
		{"/w", "/w/../w/a", true},
		{"/w", "/wx", false}, // a sibling whose name starts the same
		{"/w", "/", false},
		{"/w", "/w/../etc/passwd", false},
		{"/", "/etc", true},    // the prefix test asked for "//etc"
		{"/w", "/w/..x", true}, // a name, not a step up
		{"/w", "/w/...", true}, // likewise
		{"/w", "rel", false},   // relative against absolute: no answer is "no"
		{"w", "w/a", true},
	} {
		if got := Within(c.dir, c.p); got != c.within {
			t.Errorf("Within(%q, %q) = %v", c.dir, c.p, got)
		}
	}
	if Under("/w", "/w") || !Under("/w", "/w/a") {
		t.Error("Under counts the directory itself, or not what is beneath it")
	}
}

func FuzzWithin(f *testing.F) {
	f.Add("/w", "/w/a")
	f.Add("/", "/../x")
	f.Add("/w", "/w/..x/../../y")
	f.Fuzz(func(t *testing.T, dir, p string) {
		rel, ok := Rel(dir, p)
		if !ok {
			return
		}
		// what is within joins back to itself and never climbs
		if rel == ".." || len(rel) > 2 && rel[:3] == "../" {
			t.Fatalf("Rel(%q, %q) = %q climbs", dir, p, rel)
		}
	})
}
