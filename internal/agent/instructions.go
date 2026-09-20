package agent

import (
	"fmt"
	"os"
	"path/filepath"
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
	// Only the instructions the human trusted are followed: the files the
	// trust hash covered when the project was loaded. One that has appeared
	// since (a command an agent ran can write sub/AGENTS.md, and every agent
	// that then touches sub/ would obey it) is withheld, and the host loads
	// the project again: the hash no longer matches, so the human is asked.
	trusted := map[string]bool{}
	for _, f := range cfg.InstructionFiles {
		trusted[tools.ResolvePath("", f)] = true
	}
	var found []instructions.File // read outside the channel's lock
	untrusted := false
	for _, v := range sub.Values {
		for _, f := range instructions.Between(a.c.Dir(), tools.ResolvePath(a.c.Dir(), v)) {
			if trusted[tools.ResolvePath("", f.Path)] {
				found = append(found, f)
			} else {
				untrusted = true
			}
		}
	}
	if untrusted {
		a.c.host.ProjectChanged(a.c.Dir())
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

// trustStamp covers every file the trust hash does: the project layer
// itself — its roles, skills, commands and stavlos.json — as well as the
// instructions agents follow. Watching only the instructions left the rest
// silent: a role added or a command edited changed the hash, the project
// quietly stopped being trusted, and nothing asked again until the channel
// was resumed. Stat is cheap next to the turn this runs before.
func trustStamp(cfg *config.Effective) string {
	files := make([]string, 0, len(cfg.TrustFiles))
	for _, f := range cfg.TrustFiles {
		files = append(files, filepath.Join(cfg.Dir, f))
	}
	return instructionsStamp(files)
}

// checkProject asks the host to load the project again when anything the
// trust hash covers changed since the config was loaded: the hash no longer
// matches, so the human is asked before any agent follows the new text or
// the channel loses what the project defined.
func (c *Channel) checkProject() {
	c.mu.Lock()
	cfg, stamp := c.cfg, c.stamp
	c.mu.Unlock()
	if len(cfg.TrustFiles) == 0 || trustStamp(cfg) == stamp {
		return
	}
	c.host.ProjectChanged(c.Dir())
}
