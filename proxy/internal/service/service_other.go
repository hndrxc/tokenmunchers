//go:build !linux && !darwin

package service

import (
	"os/exec"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/config"
)

func newPlatformManager(paths config.Paths, listenAddr string) Manager {
	return &pidManager{paths: paths, listenAddr: listenAddr}
}

func detachProcess(_ *exec.Cmd) {}
