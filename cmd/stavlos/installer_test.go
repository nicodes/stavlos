//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCurlInstaller(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(file), "../../install.sh")
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "failed build preserves existing executable"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			bin, fake := filepath.Join(root, "bin"), filepath.Join(root, "commands")
			for _, dir := range []string{bin, fake} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(bin, "stavlos"), []byte("previous executable\n"), 0700); err != nil {
				t.Fatal(err)
			}
			goCommand := `#!/bin/sh
set -eu
[ "$1" = install ]
[ "$2" = 'github.com/nicodes/stavlos/cmd/stavlos@latest' ]
printf 'new executable\n' > "$GOBIN/stavlos"
if [ "${INSTALL_BUILD_FAIL:-0}" = 1 ]; then exit 42; fi
`
			if err := os.WriteFile(filepath.Join(fake, "go"), []byte(goCommand), 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", script, "--install-dir", bin)
			cmd.Env = append(os.Environ(), "PATH="+fake+string(os.PathListSeparator)+os.Getenv("PATH"))
			if fail {
				cmd.Env = append(cmd.Env, "INSTALL_BUILD_FAIL=1")
			}
			output, err := cmd.CombinedOutput()
			if fail == (err == nil) {
				t.Fatalf("unexpected result: %v\n%s", err, output)
			}
			body, err := os.ReadFile(filepath.Join(bin, "stavlos"))
			if err != nil {
				t.Fatal(err)
			}
			want := "new executable\n"
			if fail {
				want = "previous executable\n"
			}
			if string(body) != want {
				t.Fatalf("unexpected executable: %q", body)
			}
			if !fail && !strings.Contains(string(output), "Installed stavlos") {
				t.Fatalf("missing install receipt: %s", output)
			}
		})
	}
}
