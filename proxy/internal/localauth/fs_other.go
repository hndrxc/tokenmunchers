//go:build !unix

package localauth

import (
	"fmt"
	"os"
)

// openNoFollow refuses a symlink at the final component. Unlike on Unix this
// check is not atomic with the open.
func openNoFollow(path string) (*os.File, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: symlink", ErrInvalidKey)
	}
	return os.Open(path)
}

func createExclusive(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

// POSIX ownership and modes are unavailable; protection relies on the
// directory ACL inherited from the user's private profile.
func checkFile(fi os.FileInfo) error {
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: not a regular file", ErrInvalidKey)
	}
	return nil
}

func checkDir(fi os.FileInfo) error {
	if !fi.IsDir() {
		return fmt.Errorf("%w: not a directory", ErrInvalidKey)
	}
	return nil
}

func syncDir(string) error { return nil }
