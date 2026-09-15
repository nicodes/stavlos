package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// ErrAlreadyRunning: another daemon holds this data directory.
var ErrAlreadyRunning = errors.New("another stavlosd is running on this data directory")

// lockDataDir takes an exclusive advisory lock on <dataDir>/stavlosd.lock
// for the life of the returned file. Two daemons on one events.db would
// each assign sequence numbers and each steal the socket; the lock makes
// the second one fail before it opens anything. The pid inside is for
// humans looking at the directory.
func lockDataDir(dataDir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dataDir, "stavlosd.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		pid, _ := os.ReadFile(f.Name())
		f.Close()
		if len(pid) > 0 {
			return nil, fmt.Errorf("%w (pid %s)", ErrAlreadyRunning, string(pid))
		}
		return nil, ErrAlreadyRunning
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0)
	return f, nil
}
