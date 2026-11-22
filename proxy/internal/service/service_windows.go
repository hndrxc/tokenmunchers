package service

import (
	"os/exec"
	"syscall"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/config"
)

func newPlatformManager(paths config.Paths, listenAddr string) Manager {
	return &pidManager{paths: paths, listenAddr: listenAddr}
}

func detachProcess(cmd *exec.Cmd) {
	// DETACHED_PROCESS keeps the daemon off the parent's console without
	// creating a new one. The log file still receives stdout and stderr.
	const detachedProcess = 0x00000008
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess}
}
