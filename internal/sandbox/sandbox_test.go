package sandbox

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testBase is a directory outside /tmp: the sandbox mounts its own /tmp,
// which would cover t.TempDir.
func testBase(t *testing.T) string {
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Skip(err)
	}
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Skip(err)
	}
	dir, err := os.MkdirTemp(cache, "stavlos-sandbox-test-")
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestSandboxBuildAndUnixServiceContainment(t *testing.T) {
	if level, _ := Probe(); level != Full {
		t.Skip("requires full sandbox")
	}
	base := testBase(t)
	dirs := mkdirs(t, base, "work", "tmp", "neighbour")
	work, tmp, neighbour := dirs[0], dirs[1], dirs[2]
	if err := os.WriteFile(filepath.Join(work, "main.go"), []byte("package main\nfunc main() { println(\"built\") }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Fatal(err)
	}
	goRoot := strings.TrimSpace(string(root))
	spec := Spec{Writable: []string{work}, Readable: []string{goRoot}, Tmp: tmp, PrivateTmp: true, Caches: true}
	cmd := exec.Command(filepath.Join(goRoot, "bin", "go"), "run", "main.go")
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "GOENV=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOFLAGS=-buildvcs=false")
	if _, err := Wrap(cmd, spec); err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "built") {
		t.Fatalf("legitimate build: %v %s", err, out)
	}
	// The pathname is intentionally readable. A readable grant must not also
	// grant the authority of a service listening beneath that path.
	ln, err := net.Listen("unix", filepath.Join(neighbour, "service.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	spec.Readable = append(spec.Readable, neighbour)
	spec.Network = true
	cmd = exec.Command("/usr/bin/python3", "-c", "import socket; s=socket.socket(socket.AF_UNIX); s.connect("+fmt.Sprintf("%q", ln.Addr().String())+")")
	cmd.Dir = work
	if _, err := Wrap(cmd, spec); err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("ungranted Unix service reached: %s", out)
	}
}

func mkdirs(t *testing.T, base string, names ...string) []string {
	var out []string
	for _, n := range names {
		p := filepath.Join(base, n)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func TestSandbox(t *testing.T) {
	lvl, err := Probe()
	if lvl == None {
		t.Skipf("no sandbox on this system: %v", err)
	}
	t.Logf("level %s (%v)", lvl, err)
	base := testBase(t)
	d := mkdirs(t, base, "work", "outside", "hidden", "tmp", "work/.git/hooks")
	work, outside, hidden, tmp := d[0], d[1], d[2], d[3]
	os.WriteFile(filepath.Join(hidden, "secret"), []byte("s3cret"), 0o600)
	os.WriteFile(filepath.Join(outside, "f"), []byte("x"), 0o644)
	spec := Spec{Writable: []string{work}, ReadOnly: []string{filepath.Join(work, ".git", "hooks")}, Hidden: []string{hidden}, Tmp: tmp, PrivateTmp: true}
	sh := func(s Spec, script string) (string, error) {
		t.Helper()
		cmd := exec.Command("bash", "-c", script)
		cmd.Dir = work
		if _, err := Wrap(cmd, s); err != nil {
			t.Fatal(err)
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	if out, err := sh(spec, "echo ok > inside && cat inside && mkdir -p sub/x && mv inside sub/x/moved"); err != nil || out != "ok\n" {
		t.Fatalf("writing inside: %v %q", err, out)
	}
	if out, err := sh(spec, "echo no > "+filepath.Join(outside, "f")); err == nil {
		t.Fatalf("wrote outside: %q", out)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "f")); string(b) != "x" {
		t.Fatalf("outside file changed: %q", b)
	}
	if out, err := sh(spec, "p="+outside+"; bash -c 'cat \"$1/f\"' _ \"$p\""); err == nil || out == "x" {
		t.Fatalf("subprocess read outside: %v %q", err, out)
	}
	granted := spec
	granted.Readable = []string{outside}
	if out, err := sh(granted, "cat "+filepath.Join(outside, "f")); err != nil || out != "x" {
		t.Fatalf("explicit supporting read: %v %q", err, out)
	}
	if out, err := sh(granted, "echo no > "+filepath.Join(outside, "f")); err == nil {
		t.Fatalf("read grant became a write grant: %q", out)
	}
	if out, err := sh(spec, "echo $STAVLOS_SANDBOX_SPEC"); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("the spec leaked into the command's environment: %q", out)
	}

	if lvl == Full {
		if out, _ := sh(spec, "cat "+filepath.Join(hidden, "secret")); strings.Contains(out, "s3cret") {
			t.Fatal("read a hidden file")
		}
		if out, err := sh(spec, "touch "+filepath.Join(work, ".git", "hooks", "pre-commit")); err == nil {
			t.Fatalf("wrote a read-only control path: %q", out)
		}
		if out, err := sh(spec, "echo t > /tmp/x && cat /tmp/x && echo $TMPDIR"); err != nil || out != "t\n/tmp\n" {
			t.Fatalf("private tmp: %v %q", err, out)
		}
		if b, _ := os.ReadFile(filepath.Join(tmp, "x")); string(b) != "t\n" {
			t.Fatalf("the private tmp is the scratch directory: %q", b)
		}
	}

	// With the network off no TCP connection leaves the sandbox.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	connect := fmt.Sprintf("exec 3<>/dev/tcp/127.0.0.1/%d && echo connected", ln.Addr().(*net.TCPAddr).Port)
	if out, err := sh(Spec{Writable: []string{work}, Network: true}, connect); err != nil || !strings.Contains(out, "connected") {
		t.Fatalf("network on: %v %q", err, out)
	}
	if abi() >= 4 {
		if out, err := sh(Spec{Writable: []string{work}}, connect); err == nil || strings.Contains(out, "connected") {
			t.Fatalf("network off still connected: %q", out)
		}
	}
}

// TestSandboxedCommandHasNoCapabilities: the capability the helper needed
// for its mounts does not reach the command.
func TestSandboxedCommandHasNoCapabilities(t *testing.T) {
	if lvl, _ := Probe(); lvl != Full {
		t.Skip("needs user namespaces")
	}
	work := testBase(t)
	cmd := exec.Command("grep", "^Cap\\(Inh\\|Prm\\|Eff\\|Amb\\)", "/proc/self/status")
	cmd.Dir = work
	if _, err := Wrap(cmd, Spec{Writable: []string{work}}); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Fields(l); len(f) != 2 || strings.Trim(f[1], "0") != "" {
			t.Fatalf("capabilities: %s", out)
		}
	}
}
