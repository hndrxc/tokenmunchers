package desktop

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

var stateLockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")

func tryStateLock(file *os.File) error {
	const (
		lockfileFailImmediately = 0x1
		lockfileExclusiveLock   = 0x2
		errorLockViolation      = syscall.Errno(33)
	)
	if err := stateLockFileEx.Find(); err != nil {
		return errors.New("cannot lock desktop collector state")
	}
	var offset syscall.Overlapped
	ok, _, err := stateLockFileEx.Call(file.Fd(), lockfileFailImmediately|lockfileExclusiveLock,
		0, 1, 0, uintptr(unsafe.Pointer(&offset)))
	if ok != 0 {
		return nil
	}
	if errors.Is(err, errorLockViolation) {
		return errors.New("desktop collector state is already locked")
	}
	return errors.New("cannot lock desktop collector state")
}
