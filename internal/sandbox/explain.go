package sandbox

import (
	"os"
	"strings"
)

// Report is the sandbox as a person needs to hear it: whether everything it
// promises is in force, and when it is not, what is missing and what to do.
type Report struct {
	Level   Level
	Missing []string // what commands can still do that a full sandbox stops
	Why     string   // what the system said
	Fix     string   // a command that usually mends it, "" when we know of none
}

// Explain probes the system and says what it found.
func Explain() Report {
	lvl, err := Probe()
	r := Report{Level: lvl}
	if err != nil {
		r.Why = err.Error()
	}
	switch lvl {
	case Full:
	case Landlock:
		r.Missing = []string{
			"Commands can read your credentials (~/.ssh, ~/.aws, ~/.config/gh, …) and Stavlos's own files: nothing is hidden from them.",
			"Commands can edit the files that steer agents or run code later (.git/hooks, .stavlos, AGENTS.md).",
			"Commands share the system's /tmp.",
		}
		r.Fix = usernsFix()
	case None:
		r.Missing = []string{"Nothing bounds a command: it runs with your full access. Every command asks first."}
		if r.Why == "" {
			r.Why = "the kernel has no Landlock (Linux 5.13 or newer, with Landlock enabled)"
		}
	}
	return r
}

// usernsFix is the setting that most often blocks unprivileged user
// namespaces on this system, as a command that lifts it.
func usernsFix() string {
	for _, s := range []struct{ file, blocked, fix string }{
		{"/proc/sys/kernel/apparmor_restrict_unprivileged_userns", "1", "sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0"}, // Ubuntu 24.04 and later
		{"/proc/sys/kernel/unprivileged_userns_clone", "0", "sudo sysctl -w kernel.unprivileged_userns_clone=1"},                         // Debian, Arch's hardened kernel
		{"/proc/sys/user/max_user_namespaces", "0", "sudo sysctl -w user.max_user_namespaces=15000"},
	} {
		if b, err := os.ReadFile(s.file); err == nil && strings.TrimSpace(string(b)) == s.blocked {
			return s.fix
		}
	}
	return "" // a container or a locked-down host: nothing to run here
}
