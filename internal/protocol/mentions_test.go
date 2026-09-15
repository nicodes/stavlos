package protocol

import (
	"reflect"
	"testing"
)

func TestAddressees(t *testing.T) {
	for text, want := range map[string]struct {
		names   []string
		message string
	}{
		"@scout look":                           {[]string{"scout"}, "look"},
		"@Scout @lookout-2 check the tests":     {[]string{"scout", "lookout-2"}, "check the tests"},
		"@main, what about @decorators here?":   {[]string{"main"}, "what about @decorators here?"},
		"mail me@example.com":                   {nil, "mail me@example.com"},
		"hi @scout":                             {nil, "hi @scout"},
		"@scout":                                {[]string{"scout"}, ""},
		"@scout @lookout\nsecond line @x stays": {[]string{"scout", "lookout"}, "second line @x stays"},
	} {
		names, message := Addressees(text)
		if !reflect.DeepEqual(names, want.names) || message != want.message {
			t.Errorf("Addressees(%q) = %q, %q; want %q, %q", text, names, message, want.names, want.message)
		}
	}
}
