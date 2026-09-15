// Package buildid identifies the running binary so a client can notice a
// daemon built from older code (common with `go run`) and replace it.
package buildid

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"sync"
)

var (
	once sync.Once
	id   string
)

// ID returns a short hash of the executable's contents. Two binaries built
// from the same source have the same ID regardless of path, so a `go run`
// with unchanged code does not restart the daemon. It is computed on first
// use, not at init: every process the sandbox starts re-executes this
// binary, and hashing it there would cost a full read per command. The
// daemon calls it as it starts, while a `go run` binary still exists.
func ID() string {
	once.Do(func() {
		id = compute()
	})
	return id
}

func compute() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	f, err := os.Open(exe)
	if err != nil {
		return "unknown"
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}
