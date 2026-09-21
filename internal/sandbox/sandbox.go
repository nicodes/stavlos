// Package sandbox runs the processes agents start — shell commands and MCP
// servers — inside an operating-system boundary, so what the permission
// checks conclude from a command's text is also what the kernel enforces.
//
// On Linux a command is started through this binary re-executed as a small
// helper. In a private user and mount namespace the helper hides paths
// (the harness's own data, config and socket, credentials) under empty
// mounts, makes control files read-only, and gives the command its own
// /tmp; then it restricts itself with Landlock — writes only beneath the
// channel's directories, a scratch directory and caches, no TCP when the
// network is off, no signals or abstract-socket connections outside the
// sandbox — and executes the command. What the system cannot provide is
// dropped, in that order: no user namespaces means nothing hidden; no
// Landlock means no sandbox (Probe says which).
package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"

)

// helperArg0 is the helper's argv[0]: a process started with it runs the
// helper from init, before anything else in the binary.
const helperArg0 = "stavlos-sandbox"

// specEnv carries the Spec to the helper, which removes it before exec.
const specEnv = "STAVLOS_SANDBOX_SPEC"

// Spec describes one sandbox.
type Spec struct {
	Writable []string `json:"writable,omitempty"` // directories and files the process may change
	ReadOnly []string `json:"readonly,omitempty"` // paths beneath Writable that stay read-only
	Hidden   []string `json:"hidden,omitempty"`   // paths replaced by an empty directory or file
	Tmp      string   `json:"tmp,omitempty"`      // the command's scratch directory (TMPDIR), writable
	// PrivateTmp mounts Tmp over /tmp as well, where the system can: the
	// shared /tmp holds other programs' sockets (tmux, X11) and is never
	// writable from the sandbox either way.
	PrivateTmp bool `json:"private_tmp,omitempty"`
	Network    bool `json:"network,omitempty"` // TCP connections and listeners allowed
}

// wire is Spec with the unexported fields, as the helper receives it.
type wire struct {
	Spec
	Mounts bool `json:"mounts,omitempty"`
	Probe  bool `json:"probe,omitempty"`
}

// Level is how much of a Spec the system enforces.
type Level int

const (
	None     Level = iota // nothing: commands run unsandboxed
	Landlock              // writes, network and signals restricted; nothing hidden, no private /tmp
	Full                  // Landlock plus hidden paths, read-only control files and a private /tmp
)

func (l Level) String() string {
	switch l {
	case Full:
		return "full"
	case Landlock:
		return "landlock"
	case None:
	}
	return "none"
}

var probe struct {
	once  sync.Once
	level Level
	err   error
}

// Probe reports the level this system supports, finding out once by
// starting the helper at each level.
func Probe() (Level, error) {
	probe.once.Do(func() {
		if abi() <= 0 {
			probe.level, probe.err = None, errors.New("the kernel has no Landlock")
			return
		}
		if err := run(wire{Mounts: true, Probe: true}); err == nil {
			probe.level = Full
			return
		} else {
			probe.err = fmt.Errorf("user namespaces unavailable: %v", err)
		}
		if err := run(wire{Probe: true}); err == nil {
			probe.level = Landlock
			return
		}
		probe.level = None
	})
	return probe.level, probe.err
}

// run starts the helper on /bin/true with a probe spec.
func run(w wire) error {
	cmd := exec.Command("/bin/true")
	if err := wrap(cmd, w); err != nil {
		return err
	}
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) > 0 {
		return fmt.Errorf("%v: %s", err, out)
	}
	return err
}

// Wrap makes cmd, not yet started, run inside spec at the level Probe
// found. It returns the level applied; at None cmd is left unchanged.
func Wrap(cmd *exec.Cmd, spec Spec) (Level, error) {
	lvl, _ := Probe()
	if lvl == None {
		return None, nil
	}
	return lvl, wrap(cmd, wire{Spec: spec, Mounts: lvl == Full})
}

func wrap(cmd *exec.Cmd, w wire) error {
	if cmd.Err != nil {
		return cmd.Err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	b, err := json.Marshal(w)
	if err != nil {
		return err
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Args = append([]string{helperArg0, "--", cmd.Path}, cmd.Args[1:]...)
	cmd.Path = self
	cmd.Env = append(append([]string(nil), env...), specEnv+"="+string(b))
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// The namespaces are not made here. The daemon is not dumpable (so that
	// nothing of its user's can read its memory), and the kernel gives the
	// /proc files of such a process's child to root: the uid map a user
	// namespace needs could not be written, the probe failed with "permission
	// denied", and the real daemon only ever had the Landlock level while every
	// test, being dumpable, had the full one. Executing the helper makes a
	// process dumpable again, so the helper makes the namespaces for a second
	// copy of itself (helper.go, enterNamespaces).
	return nil
}
