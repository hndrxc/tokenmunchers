//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package desktop

import (
	"errors"
	"os"
	"syscall"
)

func tryStateLock(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return errors.New("desktop collector state is already locked")
		}
		return errors.New("cannot lock desktop collector state")
	}
	return nil
}
