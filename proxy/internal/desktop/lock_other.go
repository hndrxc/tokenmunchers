//go:build !windows && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package desktop

import (
	"errors"
	"os"
)

func tryStateLock(*os.File) error {
	return errors.New("desktop collector state locking is not supported on this platform")
}
