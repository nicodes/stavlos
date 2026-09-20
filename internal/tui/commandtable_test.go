package tui

import "testing"

// The palette lists a command and the table runs it: neither has one the
// other lacks, so a command cannot be offered and do nothing, or work and
// never be offered.
func TestEveryPaletteCommandRunsAndEveryRunIsListed(t *testing.T) {
	listed := map[string]bool{}
	for _, c := range commands {
		listed[c.Name] = true
		if run, ok := commandRuns[c.Name]; !ok || run.run == nil {
			t.Errorf("%s is in the palette and does nothing", c.Name)
		}
		for _, alias := range c.Aliases {
			if got, ok := commandNamed(alias); !ok || got.Name != c.Name {
				t.Errorf("alias %s does not reach %s", alias, c.Name)
			}
		}
	}
	for name := range commandRuns {
		if !listed[name] {
			t.Errorf("%s runs and is not in the palette", name)
		}
	}
}
