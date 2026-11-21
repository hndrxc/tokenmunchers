//go:build darwin

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

const launchdLabel = "com.tokenmunchers.tmproxy"

type darwinManager struct {
	paths      config.Paths
	listenAddr string
	fallback   *pidManager
}

func newPlatformManager(paths config.Paths, listenAddr string) Manager {
	return &darwinManager{
		paths:      paths,
		listenAddr: listenAddr,
		fallback:   &pidManager{paths: paths, listenAddr: listenAddr},
	}
}

func (m *darwinManager) plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

func (m *darwinManager) Backend() string { return "launchd" }

// RenderLaunchdPlist produces a macOS LaunchAgent plist for munch.
func RenderLaunchdPlist(binaryPath string, paths config.Paths, listenAddr string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>run</string>
    <string>--config-dir</string>
    <string>%s</string>
    <string>--state-dir</string>
    <string>%s</string>
    <string>--listen</string>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, launchdLabel, binaryPath, paths.ConfigDir, paths.StateDir, listenAddr, paths.LogFile(), paths.LogFile())
}

func (m *darwinManager) Install(binaryPath string) (string, error) {
	plistPath, err := m.plistPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(m.paths.StateDir, 0o700); err != nil {
		return "", err
	}
	content := RenderLaunchdPlist(binaryPath, m.paths, m.listenAddr)
	if err := os.WriteFile(plistPath, []byte(content), 0o644); err != nil {
		return "", err
	}
	_ = exec.Command("launchctl", "unload", plistPath).Run()
	if out, err := exec.Command("launchctl", "load", "-w", plistPath).CombinedOutput(); err != nil {
		return plistPath, fmt.Errorf("launchctl load: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return plistPath, nil
}

func (m *darwinManager) Uninstall() error {
	plistPath, err := m.plistPath()
	if err != nil {
		return err
	}
	_ = exec.Command("launchctl", "unload", "-w", plistPath).Run()
	if err := os.Remove(plistPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return m.fallback.Stop()
}

func (m *darwinManager) Start(binaryPath string) error {
	plistPath, err := m.plistPath()
	if err == nil {
		if _, statErr := os.Stat(plistPath); statErr == nil {
			if out, err := exec.Command("launchctl", "load", "-w", plistPath).CombinedOutput(); err != nil {
				return fmt.Errorf("launchctl load: %v (%s)", err, strings.TrimSpace(string(out)))
			}
			return nil
		}
	}
	return m.fallback.Start(binaryPath)
}

func (m *darwinManager) Stop() error {
	plistPath, err := m.plistPath()
	if err == nil {
		if _, statErr := os.Stat(plistPath); statErr == nil {
			_ = exec.Command("launchctl", "unload", plistPath).Run()
			return nil
		}
	}
	return m.fallback.Stop()
}

func (m *darwinManager) Status() Info {
	plistPath, _ := m.plistPath()
	_, installed := os.Stat(plistPath)
	return Info{
		Backend:   "launchd",
		Installed: installed == nil,
		Running:   probeDaemon(context.Background(), m.paths, m.listenAddr),
		UnitPath:  plistPath,
	}
}

func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
