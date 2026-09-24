package sandbox

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// specOf is the wire the helper would read from cmd's environment.
func specOf(t *testing.T, cmd *exec.Cmd) wire {
	t.Helper()
	var w wire
	for _, kv := range cmd.Env {
		if v, ok := strings.CutPrefix(kv, specEnv+"="); ok {
			if err := json.Unmarshal([]byte(v), &w); err != nil {
				t.Fatalf("spec: %v", err)
			}
			return w
		}
	}
	t.Fatalf("no %s in %v", specEnv, cmd.Env)
	return w
}

// TestWrapRoutesTheCommandThroughTheHelper: the command becomes this
// binary run as the helper, its own path and arguments after "--", with the
// spec in its environment and nothing else about it changed.
func TestWrapRoutesTheCommandThroughTheHelper(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	w := wire{Spec: Spec{Writable: []string{"/w"}, ReadOnly: []string{"/w/ro"}, Hidden: []string{"/h"}, Tmp: "/t", PrivateTmp: true, Network: true}, Mounts: true}
	cmd := exec.Command("/bin/echo", "a", "b c")
	path := cmd.Path
	if err := wrap(cmd, w); err != nil {
		t.Fatal(err)
	}
	if want := []string{helperArg0, "--", path, "a", "b c"}; !slices.Equal(cmd.Args, want) {
		t.Errorf("Args %q, want %q", cmd.Args, want)
	}
	if cmd.Path != self {
		t.Errorf("Path %q, want this binary %q", cmd.Path, self)
	}
	if got := specOf(t, cmd); !reflect.DeepEqual(got, w) {
		t.Errorf("the spec did not round-trip: %+v, want %+v", got, w)
	}
	if cmd.SysProcAttr == nil {
		t.Error("no SysProcAttr")
	}
}

// TestWrapEnvironment: a nil environment is the process's own; a given one
// is copied, not aliased, with the spec appended.
func TestWrapEnvironment(t *testing.T) {
	cmd := exec.Command("/bin/true")
	if err := wrap(cmd, wire{}); err != nil {
		t.Fatal(err)
	}
	for _, kv := range os.Environ() {
		if !slices.Contains(cmd.Env, kv) {
			t.Errorf("nil env: %q missing", kv)
		}
	}
	env := make([]string, 1, 4) // room to append in place, were it aliased
	env[0] = "A=1"
	cmd = exec.Command("/bin/true")
	cmd.Env = env
	if err := wrap(cmd, wire{}); err != nil {
		t.Fatal(err)
	}
	if len(cmd.Env) != 2 || cmd.Env[0] != "A=1" || !strings.HasPrefix(cmd.Env[1], specEnv+"=") {
		t.Errorf("env: %q", cmd.Env)
	}
	cmd.Env[0] = "B=2"
	if env[0] != "A=1" {
		t.Errorf("the caller's environment was aliased: %q", env)
	}
	if env[:2][1] != "" {
		t.Errorf("the caller's environment was appended to in place: %q", env[:2])
	}
}

// TestWrapKeepsSysProcAttrAndErr: an attribute set before is kept, and a
// command that could not be found fails before anything is changed.
func TestWrapKeepsSysProcAttrAndErr(t *testing.T) {
	attr := &syscall.SysProcAttr{Setpgid: true}
	cmd := exec.Command("/bin/true")
	cmd.SysProcAttr = attr
	if err := wrap(cmd, wire{}); err != nil || cmd.SysProcAttr != attr {
		t.Errorf("SysProcAttr replaced: %v %v", cmd.SysProcAttr, err)
	}
	cmd = exec.Command("stavlos-no-such-program-4f2c")
	if cmd.Err == nil {
		t.Skip("exec.Command found the program")
	}
	args := slices.Clone(cmd.Args)
	if err := wrap(cmd, wire{}); !errors.Is(err, cmd.Err) {
		t.Errorf("wrap: %v, want the command's %v", err, cmd.Err)
	}
	if !slices.Equal(cmd.Args, args) || cmd.Env != nil {
		t.Errorf("a failed wrap changed the command: %q %q", cmd.Args, cmd.Env)
	}
}

// TestSetEnvReplacesOrAdds: one variable is set, whatever was there.
func TestSetEnvReplacesOrAdds(t *testing.T) {
	env := []string{"A=1", "TMPDIR=/old", "B=2"}
	if got := setEnv(env, "TMPDIR", "/new"); !slices.Equal(got, []string{"A=1", "B=2", "TMPDIR=/new"}) {
		t.Errorf("replace: %q", got)
	}
	if got := setEnv(env, "C", "3"); !slices.Equal(got, []string{"A=1", "TMPDIR=/old", "B=2", "C=3"}) {
		t.Errorf("add: %q", got)
	}
	if !slices.Equal(env, []string{"A=1", "TMPDIR=/old", "B=2"}) {
		t.Errorf("the input was changed: %q", env)
	}
	if got := setEnv(nil, "A", "1"); !slices.Equal(got, []string{"A=1"}) {
		t.Errorf("nil: %q", got)
	}
}

// TestLevelString: every level has a name, and an unknown one is "none".
func TestLevelString(t *testing.T) {
	for l, want := range map[Level]string{None: "none", Landlock: "landlock", Full: "full", Level(42): "none"} {
		if got := l.String(); got != want {
			t.Errorf("Level(%d).String() = %q, want %q", int(l), got, want)
		}
	}
}

// runHelper runs this test binary as the helper (argv[0] is helperArg0, so
// its init runs the helper and exits) with args and the given spec, or with
// none when spec is "".
func runHelper(t *testing.T, spec string, args ...string) (stderr string, code int) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, specEnv+"=") || strings.HasPrefix(kv, stageEnv+"=")
	})
	if spec != "" {
		env = append(env, specEnv+"="+spec)
	}
	cmd := &exec.Cmd{Path: self, Args: append([]string{helperArg0}, args...), Env: env}
	var errb strings.Builder
	cmd.Stderr = &errb
	err = cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return errb.String(), 0
	case errors.As(err, &exit):
		return errb.String(), exit.ExitCode()
	}
	t.Fatal(err)
	return "", -1
}

// TestHelperRefusesABadSpecOrMissingSeparator: without a spec the helper
// says so; with one but no "--" it prints its usage. Both exit 126, the
// shell's "cannot execute", before touching the kernel.
func TestHelperRefusesABadSpecOrMissingSeparator(t *testing.T) {
	if stderr, code := runHelper(t, "", "--", "/bin/true"); code != 126 || !strings.Contains(stderr, "bad spec") {
		t.Errorf("no spec: %d %q", code, stderr)
	}
	if stderr, code := runHelper(t, "{not json", "--", "/bin/true"); code != 126 || !strings.Contains(stderr, "bad spec") {
		t.Errorf("bad spec: %d %q", code, stderr)
	}
	if stderr, code := runHelper(t, "{}", "/bin/true"); code != 126 || !strings.Contains(stderr, "usage: "+helperArg0+" --") {
		t.Errorf("no separator: %d %q", code, stderr)
	}
	if stderr, code := runHelper(t, "{}", "--"); code != 126 || !strings.Contains(stderr, "usage:") {
		t.Errorf("no program: %d %q", code, stderr)
	}
}
