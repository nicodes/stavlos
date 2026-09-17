package agent

import (
	"fmt"
	"os"
	"strings"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/instructions"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/tools"
)

// instructionsFor is the note a file tool's result carries when the agent
// first works in a directory below the channel directory that has its own
// instructions (those from the repository root down to the channel
// directory are in the system prompt already). Only a trusted project's
// files count, and each is attached once until a compaction summarises the
// results that carried it.
func (a *Agent) instructionsFor(sub policy.Subject, cfg *config.Effective) string {
	if sub.Kind != policy.KindPath || !cfg.ProjectTrusted {
		return ""
	}
	var found []instructions.File // read outside the channel's lock
	for _, v := range sub.Values {
		found = append(found, instructions.Between(a.c.Dir(), tools.ResolvePath(a.c.Dir(), v))...)
	}
	if len(found) == 0 {
		return ""
	}
	a.c.mu.Lock()
	var files []instructions.File
	for _, f := range found {
		if a.instructed[f.Path] {
			continue
		}
		if a.instructed == nil {
			a.instructed = map[string]bool{}
		}
		a.instructed[f.Path] = true
		files = append(files, f)
	}
	a.c.mu.Unlock()
	return instructions.Note(files, instructions.Budget)
}

// instructionsStamp is the size and modification time of each file, enough
// to notice an edit without reading them.
func instructionsStamp(files []string) string {
	var b strings.Builder
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			fmt.Fprintf(&b, "%s\x00gone\x00", f)
			continue
		}
		fmt.Fprintf(&b, "%s\x00%d\x00%d\x00", f, st.Size(), st.ModTime().UnixNano())
	}
	return b.String()
}

// checkInstructions asks the host to load the project again when one of its
// instructions files changed since the config was loaded: the trust hash no
// longer matches, so the human is asked before any agent follows the new
// text.
func (c *Channel) checkInstructions() {
	c.mu.Lock()
	files, stamp := c.cfg.InstructionFiles, c.stamp
	c.mu.Unlock()
	if len(files) == 0 || instructionsStamp(files) == stamp {
		return
	}
	c.host.ProjectChanged(c.Dir())
}
