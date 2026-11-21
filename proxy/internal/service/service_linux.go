//go:build linux

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/config"
)

const systemdUnitName = "tmproxy.service"

type linuxManager struct {
	paths      config.Paths
	listenAddr string
	fallback   *pidManager
}

func newPlatformManager(paths config.Paths, listenAddr string) Manager {
	return &linuxManager{
		paths:      paths,
		listenAddr: listenAddr,
		fallback:   &pidManager{paths: paths, listenAddr: listenAddr},
	}
}

func (m *linuxManager) unitPath() (string, error) {
	base := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user", systemdUnitName), nil
}

func (m *linuxManager) hasSystemdUser() bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	cmd := exec.Command("systemctl", "--user", "show-environment")
	return cmd.Run() == nil
}

func (m *linuxManager) unitInstalled() (string, bool) {
	p, err := m.unitPath()
	if err != nil {
		return "", false
	}
	_, err = os.Stat(p)
	return p, err == nil
}

func (m *linuxManager) Backend() string {
	if m.hasSystemdUser() {
		return "systemd-user"
	}
	return m.fallback.Backend()
}

// RenderSystemdUnit produces the systemd --user service definition.
func RenderSystemdUnit(binaryPath string, paths config.Paths, listenAddr string) string {
	return fmt.Sprintf(`[Unit]
Description=TokenMunchers local inference usage proxy (munch)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%q run --config-dir %q --state-dir %q --listen %q
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
`, binaryPath, paths.ConfigDir, paths.StateDir, listenAddr)
}

func (m *linuxManager) Install(binaryPath string) (string, error) {
	unitPath, err := m.unitPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return "", fmt.Errorf("create systemd user dir: %w", err)
	}
	content := RenderSystemdUnit(binaryPath, m.paths, m.listenAddr)
	if err := os.WriteFile(unitPath, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write unit file: %w", err)
	}
	if m.hasSystemdUser() {
		if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
			return unitPath, fmt.Errorf("systemctl daemon-reload: %v (%s)", err, strings.TrimSpace(string(out)))
		}
		if out, err := exec.Command("systemctl", "--user", "enable", "--now", systemdUnitName).CombinedOutput(); err != nil {
			return unitPath, fmt.Errorf("systemctl enable --now: %v (%s)", err, strings.TrimSpace(string(out)))
		}
	}
	return unitPath, nil
}

func (m *linuxManager) Uninstall() error {
	if m.hasSystemdUser() {
		_ = exec.Command("systemctl", "--user", "disable", "--now", systemdUnitName).Run()
	}
	unitPath, err := m.unitPath()
	if err != nil {
		return err
	}
	if err := os.Remove(unitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if m.hasSystemdUser() {
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	}
	return m.fallback.Stop()
}

func (m *linuxManager) Start(binaryPath string) error {
	if _, installed := m.unitInstalled(); installed && m.hasSystemdUser() {
		if out, err := exec.Command("systemctl", "--user", "start", systemdUnitName).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl start: %v (%s)", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	return m.fallback.Start(binaryPath)
}

func (m *linuxManager) Stop() error {
	if _, installed := m.unitInstalled(); installed && m.hasSystemdUser() {
		if out, err := exec.Command("systemctl", "--user", "stop", systemdUnitName).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl stop: %v (%s)", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	return m.fallback.Stop()
}

func (m *linuxManager) Status() Info {
	unitPath, installed := m.unitInstalled()
	running := probeDaemon(context.Background(), m.paths, m.listenAddr)
	if installed && m.hasSystemdUser() {
		out, _ := exec.Command("systemctl", "--user", "is-active", systemdUnitName).CombinedOutput()
		state := strings.TrimSpace(string(out))
		return Info{
			Backend:   "systemd-user",
			Installed: true,
			Running:   running || state == "active",
			UnitPath:  unitPath,
			Detail:    state,
		}
	}
	info := m.fallback.Status()
	info.Installed = installed
	if installed {
		info.UnitPath = unitPath
	}
	return info
}

func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
