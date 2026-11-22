package service

import (
	"fmt"
	"math"
	"os/exec"
	"syscall"
	"time"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/config"
)

func newPlatformManager(paths config.Paths, listenAddr string) Manager {
	return &pidManager{paths: paths, listenAddr: listenAddr, openProcessWait: openWindowsProcessWait}
}

func detachProcess(cmd *exec.Cmd) {
	// DETACHED_PROCESS keeps the daemon off the parent's console without
	// creating a new one. The log file still receives stdout and stderr.
	const detachedProcess = 0x00000008
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess}
}

type windowsProcessWaiter syscall.Handle

func openWindowsProcessWait(pid int) (processExitWaiter, error) {
	if uint64(pid) > math.MaxUint32 {
		return nil, fmt.Errorf("PID is out of Windows process ID range")
	}
	// SYNCHRONIZE permits waiting, not querying or terminating the process.
	handle, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return nil, err
	}
	return windowsProcessWaiter(handle), nil
}

func (p windowsProcessWaiter) wait() error {
	// Allow for the HTTP drain (10s), reporter close (15s), and final flush
	// (5s), plus some margin. Listener closure can precede all of these.
	const timeout = 35 * time.Second
	event, err := syscall.WaitForSingleObject(syscall.Handle(p), uint32(timeout/time.Millisecond))
	if err != nil {
		return fmt.Errorf("wait for process exit: %w", err)
	}
	switch event {
	case syscall.WAIT_OBJECT_0:
		return nil
	case syscall.WAIT_TIMEOUT:
		return fmt.Errorf("did not exit within %s", timeout)
	default:
		return fmt.Errorf("unexpected process wait result %#x", event)
	}
}

func (p windowsProcessWaiter) close() {
	_ = syscall.CloseHandle(syscall.Handle(p))
}
