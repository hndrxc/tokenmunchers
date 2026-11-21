//go:build unix

package localauth

import (
	"fmt"
	"os"
	"syscall"
)

// openNoFollow opens path read-only without following a final symlink and
// without blocking on FIFOs; callers must still verify the file type.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

// createExclusive creates a new 0600 file, failing if the name exists
// (including as a symlink).
func createExclusive(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	// Defeat any permissive umask/default ACL surprises.
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return f, nil
}

func ownedByCurrentUser(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int64(st.Uid) == int64(os.Geteuid())
}

// checkFile requires an owner-only regular file owned by the current user.
func checkFile(fi os.FileInfo) error {
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: not a regular file", ErrInvalidKey)
	}
	if !ownedByCurrentUser(fi) {
		return fmt.Errorf("%w: file not owned by current user", ErrInvalidKey)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: file permissions too open (want 0600)", ErrInvalidKey)
	}
	return nil
}

// checkDir requires a directory owned by the current user that others cannot
// modify (which would allow replacing the key file).
func checkDir(fi os.FileInfo) error {
	if !fi.IsDir() {
		return fmt.Errorf("%w: not a directory", ErrInvalidKey)
	}
	if !ownedByCurrentUser(fi) {
		return fmt.Errorf("%w: directory not owned by current user", ErrInvalidKey)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: directory is group- or world-writable", ErrInvalidKey)
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
