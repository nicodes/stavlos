package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/tools"
)

// The control files (sandboxReadOnly, the instruction files the trust hash
// knows) are what a command must not change: git's hooks and config run
// code the next time the user runs git outside the sandbox, .envrc the next
// time a shell enters the directory, .stavlos and AGENTS.md steer the
// agents. The sandbox mounts them read-only where the kernel allows a mount
// namespace; where it does not (the limited level) nothing stops a write,
// and even with the mounts a command can rename the parent directory out
// from under one and recreate it. So they are also stamped before every
// command and compared after it, and a change is said out loud in the
// result the agent and the human both read. Detection, not prevention: the
// prevention is the sandbox, and the user's review before running git.

// controlStamp is the content hash of each control path, "" for one that
// does not exist.
type controlStamp map[string]string

// controlLimit bounds how much of a control directory is hashed.
const controlLimit = 200

// stampControl hashes the control files under the working directories.
func (c *Channel) stampControl(cfg *config.Effective) controlStamp {
	st := controlStamp{}
	for _, p := range c.controlPaths(cfg) {
		st[p] = hashPath(p)
	}
	return st
}

// controlPaths are the control files and directories of every working
// directory, plus the instruction files the trust hash covers.
func (c *Channel) controlPaths(cfg *config.Effective) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		p = tools.ResolvePath("", p)
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, d := range c.dirPaths() {
		for _, ro := range sandboxReadOnly {
			add(filepath.Join(d, ro))
		}
	}
	for _, f := range cfg.InstructionFiles {
		add(f)
	}
	return out
}

// controlNote compares the control files with a stamp and names the ones
// that changed, "" when none did. A nil stamp is nothing to compare.
func (c *Channel) controlNote(before controlStamp, cfg *config.Effective) string {
	if before == nil {
		return ""
	}
	var changed []string
	for p, was := range before {
		if now := hashPath(p); now != was {
			changed = append(changed, p)
		}
	}
	if len(changed) == 0 {
		return ""
	}
	sort.Strings(changed)
	log.Printf("channel %s: a command changed control files: %s", c.ID, strings.Join(changed, ", "))
	return "[this command changed " + strings.Join(changed, ", ") + ", which steers the harness or runs code later. The human should review the change before git or a shell runs it; do not do that yourself.]"
}

// hashPath is a content hash of a file, or of a directory's files (names
// and contents, at most controlLimit of them), "" when it does not exist.
func hashPath(p string) string {
	st, err := os.Lstat(p)
	if err != nil {
		return ""
	}
	h := sha256.New()
	if !st.IsDir() {
		h.Write([]byte(st.Mode().String()))
		if b, err := os.ReadFile(p); err == nil {
			h.Write(b)
		}
		return hex.EncodeToString(h.Sum(nil))
	}
	n := 0
	_ = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == p {
			return nil
		}
		if n++; n > controlLimit {
			return filepath.SkipAll
		}
		h.Write([]byte(path))
		h.Write([]byte{0})
		if info, err := d.Info(); err == nil {
			h.Write([]byte(info.Mode().String()))
		}
		if d.Type().IsRegular() {
			if b, err := os.ReadFile(path); err == nil {
				h.Write(b)
			}
		}
		h.Write([]byte{0})
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))
}
