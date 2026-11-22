package desktop

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

var errStateLockStorage = errors.New("cannot open desktop collector lock")

// acquireStateLock holds the lock until the returned file is closed.
func acquireStateLock(dir string) (*os.File, error) {
	path := filepath.Join(dir, "collector.lock")
	before, err := os.Lstat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) || err == nil && !before.Mode().IsRegular() {
		return nil, errStateLockStorage
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, errStateLockStorage
	}
	opened, statErr := file.Stat()
	current, pathErr := os.Lstat(path)
	if statErr != nil || pathErr != nil || !opened.Mode().IsRegular() || !current.Mode().IsRegular() ||
		!os.SameFile(opened, current) || (before != nil && !os.SameFile(before, opened)) {
		_ = file.Close()
		return nil, errStateLockStorage
	}
	if err := tryStateLock(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}
