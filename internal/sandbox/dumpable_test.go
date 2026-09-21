package sandbox

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// The daemon is not dumpable, and the kernel gives the /proc files of such a
// process's children to root, so it could not write the uid map of a user
// namespace it made for a child: in the real daemon the full sandbox failed
// with "permission denied" and fell back to Landlock alone, while every test,
// being dumpable, had the full one. This runs the probe as the daemon does.
func TestTheFullSandboxWorksFromAProcessThatIsNotDumpable(t *testing.T) {
	if os.Getenv("STAVLOS_TEST_NOT_DUMPABLE") == "1" {
		if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
		if err := run(wire{Mounts: true, Probe: true}); err != nil {
			t.Fatalf("FULL-SANDBOX-FAILED: %v", err)
		}
		return
	}
	if abi() <= 0 {
		t.Skip("no Landlock on this kernel")
	}
	if err := run(wire{Mounts: true, Probe: true}); err != nil {
		t.Skipf("no user namespaces here even when dumpable: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run", "^TestTheFullSandboxWorksFromAProcessThatIsNotDumpable$", "-test.v")
	cmd.Env = append(os.Environ(), "STAVLOS_TEST_NOT_DUMPABLE=1")
	out, err := cmd.CombinedOutput()
	if err != nil || strings.Contains(string(out), "FULL-SANDBOX-FAILED") {
		t.Fatalf("as the daemon runs (not dumpable), the full sandbox is unavailable: %v\n%s", err, out)
	}
}
