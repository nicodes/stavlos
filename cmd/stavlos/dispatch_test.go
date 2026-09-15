package main

import (
	"reflect"
	"testing"
)

// TestCommandOf: command words and the dash-spelled commands reach their
// handler; other leading flags start a session; unknown words stay unknown.
func TestCommandOf(t *testing.T) {
	for _, c := range []struct {
		args     []string
		cmd      string
		rest     []string
		dispatch bool
	}{
		{nil, "", nil, true},
		{[]string{"--version"}, "--version", []string{}, true},
		{[]string{"version"}, "version", []string{}, true},
		{[]string{"--help"}, "--help", []string{}, true},
		{[]string{"-h"}, "-h", []string{}, true},
		{[]string{"--model", "openai/gpt-5.4"}, "", []string{"--model", "openai/gpt-5.4"}, true},
		{[]string{"resume", "s1"}, "resume", []string{"s1"}, true},
		{[]string{"bogus"}, "bogus", []string{}, false},
	} {
		cmd, rest := commandOf(c.args)
		if cmd != c.cmd || (len(rest) != 0 || len(c.rest) != 0) && !reflect.DeepEqual(rest, c.rest) {
			t.Errorf("%q: got %q %q, want %q %q", c.args, cmd, rest, c.cmd, c.rest)
		}
		if _, ok := subcommands[cmd]; ok != c.dispatch {
			t.Errorf("%q: dispatches=%v", c.args, ok)
		}
	}
}
