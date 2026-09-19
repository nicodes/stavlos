// Package statefile writes the small files Stavlos keeps beside its event
// log and in its configuration directory. There was a hand-written
// temp-and-rename in nine places, three plain os.WriteFile calls that could
// leave half a file, and none of them synced; this is the one way.
package statefile

import (
	"os"
	"path/filepath"
)

// WriteAtomic replaces path with data, or leaves it exactly as it was. The
// data goes to a temporary file in the same directory, is synced, and is
// renamed over path; the directory is synced too, so the rename survives a
// crash. mode is the file's mode when it is created; an existing file keeps
// its own when keepMode is set (a project's stavlos.json is a committed file,
// and changing its mode would show up in git).
func WriteAtomic(path string, data []byte, mode os.FileMode, keepMode bool) error {
	dir := filepath.Dir(path)
	if keepMode {
		if st, err := os.Stat(path); err == nil {
			mode = st.Mode().Perm()
		}
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op once renamed
	if _, err = f.Write(data); err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
