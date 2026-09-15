package protocol

import (
	"reflect"
	"testing"
)

func TestMentions(t *testing.T) {
	for text, want := range map[string][]string{
		"@scout look": {"scout"},
		"hey @Scout and @lookout-2, @scout again": {"scout", "lookout-2"},
		"mail me@example.com":                     nil,
		"(@main) @kid-. @ alone @":                {"main", "kid"},
		"no mentions here":                        nil,
	} {
		if got := Mentions(text); !reflect.DeepEqual(got, want) {
			t.Errorf("Mentions(%q) = %q, want %q", text, got, want)
		}
	}
}
