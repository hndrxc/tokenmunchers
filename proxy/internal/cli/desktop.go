package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/claudesettings"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/config"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/desktop"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/localauth"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/proxy"
)

func desktopDir(paths config.Paths) string {
	return filepath.Join(paths.StateDir, "desktop")
}

func desktopEnabled(cfg config.Config) bool {
	return !cfg.DesktopClaudeSince.IsZero() || !cfg.DesktopCodexSince.IsZero()
}

func (a *App) cmdDesktop(args []string) error {
	fs := flag.NewFlagSet("desktop", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	cfgDir, stateDir := addPathFlags(fs)
	listen := fs.String("listen", "", "override listen address for claude-env")
	fs.Usage = func() {
		fmt.Fprintln(a.Stderr, "Usage: munch desktop [claude|codex] [on|off] or munch desktop claude-env")
		fs.PrintDefaults()
	}
	// Accept path flags both before and after a complete subcommand.
	var command []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = append(command, args[0]), args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	command = append(command, fs.Args()...)
	paths, cfg, err := loadPathsAndConfig(*cfgDir, *stateDir, *listen)
	if err != nil {
		return err
	}

	switch {
	case len(command) == 0:
		fmt.Fprintf(a.Stdout, "Claude Desktop: %s\nCodex Desktop:  %s\n", desktopState(cfg.DesktopClaudeSince), desktopState(cfg.DesktopCodexSince))
		fmt.Fprintln(a.Stdout, "Turn a source on or off with `munch desktop claude on|off` or `munch desktop codex on|off`, then run `munch restart`. Only live usage is collected.")
		fmt.Fprintln(a.Stdout, "For Claude, munch adds or removes its telemetry settings in Claude's settings.json; restart Claude Desktop afterwards.")
		fmt.Fprintln(a.Stdout, "If munch can't edit that file, `munch desktop claude-env` prints the settings to merge by hand.")
		return nil
	case len(command) == 1 && command[0] == "claude-env":
		return a.printClaudeDesktopEnv(paths, cfg)
	case len(command) == 2 && (command[0] == "claude" || command[0] == "codex") && (command[1] == "on" || command[1] == "off"):
		return a.setDesktopSource(paths, cfg, command[0], command[1] == "on")
	default:
		return errors.New("usage: munch desktop [claude|codex] [on|off] or munch desktop claude-env")
	}
}

func desktopState(since time.Time) string {
	if since.IsZero() {
		return "off"
	}
	return "on since " + since.UTC().Format(time.RFC3339)
}

// setDesktopSourceQuiet records the opt-in state of a desktop source in
// config.json without printing anything. It is shared by `munch desktop` and
// install; cfg is updated to mirror what was saved.
func setDesktopSourceQuiet(cfg *config.Config, paths config.Paths, source string, on bool) error {
	if on {
		if err := config.ValidateAPIKey(cfg.APIKey); err != nil {
			return errors.New("desktop collection requires a configured TokenMunchers key; run `munch login`")
		}
		if err := desktop.ValidateEndpoint(cfg.IngestURL); err != nil {
			return err
		}
	}
	disk, err := config.LoadFile(paths)
	if err != nil {
		return err
	}
	since := &disk.DesktopClaudeSince
	if source == "codex" {
		since = &disk.DesktopCodexSince
	}
	if since.IsZero() == on {
		if on {
			if disk.Paused {
				dir := desktopDir(paths)
				if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
					if err := persistDesktopPause(dir, true, true); err != nil {
						return err
					}
				}
			}
			*since = time.Now().UTC()
		} else {
			*since = time.Time{}
		}
		if err := config.Save(paths, disk); err != nil {
			return err
		}
	}
	cfg.DesktopClaudeSince, cfg.DesktopCodexSince = disk.DesktopClaudeSince, disk.DesktopCodexSince
	return nil
}

// claudeEndpoint returns the secret OTLP logs endpoint Claude Desktop exports to.
func claudeEndpoint(listen string, key localauth.Key) string {
	return "http://" + listen + key.RoutePrefix() + proxy.ClaudeLogsPath
}

// claudeSettingsPath returns Claude's user settings.json path.
func claudeSettingsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return claudesettings.Path(claudesettings.Dir(os.Getenv, home))
}

// claudeSettingsPlan inspects Claude's settings.json for the telemetry
// settings that point Claude Desktop at this proxy.
func claudeSettingsPlan(paths config.Paths, cfg config.Config) (*claudesettings.Plan, error) {
	if _, port, err := net.SplitHostPort(cfg.ListenAddr); err != nil || port == "0" {
		return nil, errors.New("Claude Desktop collection requires a fixed nonzero Munch listen port")
	}
	key, err := localauth.LoadOrCreate(paths.ConfigDir)
	if err != nil {
		return nil, err
	}
	return claudesettings.Inspect(claudeSettingsPath(), claudeEndpoint(cfg.ListenAddr, key))
}

// claudeSettingsBlocked says why munch left Claude's settings.json alone.
func claudeSettingsBlocked(plan *claudesettings.Plan) string {
	return fmt.Sprintf("Claude Desktop not set up: left %s as is because %s. Set it up by hand with `munch desktop claude-env`.", plan.Path, plan.Reason)
}

// writeClaudeDesktopSettings adds Munch's telemetry settings to Claude's
// settings.json, or repoints them at this proxy. A settings file munch must
// not edit gets its reason and the manual fallback printed instead.
func (a *App) writeClaudeDesktopSettings(paths config.Paths, cfg config.Config) error {
	plan, err := claudeSettingsPlan(paths, cfg)
	if err != nil {
		return err
	}
	switch {
	case plan.Status == claudesettings.Blocked:
		fmt.Fprintln(a.Stdout, claudeSettingsBlocked(plan))
	case plan.NeedsWrite():
		if err := claudesettings.Apply(plan); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "Added munch's telemetry settings to %s. Restart Claude Desktop and start a new local Code session.\n", plan.Path)
	}
	return nil
}

// removeClaudeDesktopSettings removes Munch's telemetry keys from Claude's
// settings.json and says what it left behind.
func (a *App) removeClaudeDesktopSettings() error {
	path := claudeSettingsPath()
	res, err := claudesettings.Remove(path)
	if err != nil {
		return err
	}
	if res.Removed {
		fmt.Fprintf(a.Stdout, "Removed munch's telemetry settings from %s.\n", path)
	}
	if len(res.Kept) > 0 {
		fmt.Fprintf(a.Stdout, "%s still has telemetry settings that differ from what munch wrote (%s), so they were left alone.\n", path, strings.Join(res.Kept, ", "))
	}
	if res.Removed || len(res.Kept) > 0 {
		fmt.Fprintln(a.Stdout, "Restart Claude Desktop.")
	}
	return nil
}

// setDesktopSource configures a desktop source and prints the result.
func (a *App) setDesktopSource(paths config.Paths, cfg config.Config, source string, on bool) error {
	if err := setDesktopSourceQuiet(&cfg, paths, source, on); err != nil {
		return err
	}
	since := cfg.DesktopClaudeSince
	if source == "codex" {
		since = cfg.DesktopCodexSince
	}
	fmt.Fprintf(a.Stdout, "%s desktop collection configured %s. Restart Munch (`munch restart`).\n", source, desktopState(since))
	switch {
	case source != "claude":
		return nil
	case on:
		return a.writeClaudeDesktopSettings(paths, cfg)
	default:
		return a.removeClaudeDesktopSettings()
	}
}

// installDesktopSupported gates the install desktop step: Claude Desktop and
// the Codex desktop app are only detected on Windows. Tests may enable it.
var installDesktopSupported = runtime.GOOS == "windows"

// desktopOutcome says what the install desktop step changed.
type desktopOutcome struct {
	// claudeSettingsChanged is set when Claude's settings.json was written this
	// run, so Claude Desktop must be restarted to pick it up.
	claudeSettingsChanged bool
}

// claudeDesktopDetected reports whether Claude Desktop is installed: the
// MSIX package, its roaming data directory, or the older per-user install.
func claudeDesktopDetected() bool {
	return packageDirExists("Claude_*") ||
		isDir(filepath.Join(os.Getenv("APPDATA"), "Claude")) ||
		isDir(filepath.Join(os.Getenv("LOCALAPPDATA"), "AnthropicClaude"))
}

// codexDesktopDetected reports whether the Codex desktop app or ChatGPT
// Desktop is installed. OpenAI.CodexPrimaryRuntime.* packages don't count.
func codexDesktopDetected() bool {
	return packageDirExists("OpenAI.Codex_*") || packageDirExists("OpenAI.ChatGPT-Desktop_*")
}

// packageDirExists reports whether an MSIX package directory matching pattern
// exists under %LOCALAPPDATA%\Packages.
func packageDirExists(pattern string) bool {
	root := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
	if root == "" {
		return false
	}
	matches, _ := filepath.Glob(filepath.Join(root, "Packages", pattern))
	return slices.ContainsFunc(matches, isDir)
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// installDesktop offers usage collection for each desktop app it detects. It
// runs before the service step so the daemon starts with the opt-ins, and is
// silent when no desktop app is detected.
func (a *App) installDesktop(p *prompter, paths config.Paths, cfg config.Config) (desktopOutcome, error) {
	var out desktopOutcome
	claude, codexApp := claudeDesktopDetected(), codexDesktopDetected()
	if !claude && !codexApp {
		return out, nil
	}
	if err := desktop.ValidateEndpoint(cfg.IngestURL); err != nil {
		a.stepf("Desktop", "desktop app usage can't be counted with this ingest URL: %v", err)
		return out, nil
	}
	if claude {
		changed, err := a.installClaudeDesktop(p, paths, &cfg)
		if err != nil {
			return out, err
		}
		out.claudeSettingsChanged = changed
	}
	if codexApp {
		if err := a.installCodexDesktop(p, paths, &cfg); err != nil {
			return out, err
		}
	}
	return out, nil
}

// installClaudeDesktop asks to count Claude Desktop usage unless the user
// already opted in, and keeps Munch's telemetry settings in settings.json
// pointed at this proxy. It reports whether settings.json changed.
func (a *App) installClaudeDesktop(p *prompter, paths config.Paths, cfg *config.Config) (bool, error) {
	plan, err := claudeSettingsPlan(paths, *cfg)
	if err != nil {
		fmt.Fprintf(a.Stderr, "warning: Claude Desktop was not set up: %v\n", err)
		return false, nil
	}
	if plan.Status == claudesettings.Blocked {
		a.stepf("Desktop", "%s", claudeSettingsBlocked(plan))
		return false, nil
	}
	if cfg.DesktopClaudeSince.IsZero() {
		ok, err := p.confirm("Count Claude Desktop usage? Munch adds telemetry settings to " + plan.Path + ".")
		if err != nil {
			return false, err
		}
		if !ok {
			a.stepf("Desktop", "Claude Desktop usage is not counted")
			return false, nil
		}
		if err := setDesktopSourceQuiet(cfg, paths, "claude", true); err != nil {
			return false, err
		}
	}
	if err := claudesettings.Apply(plan); err != nil {
		fmt.Fprintf(a.Stderr, "warning: could not update %s, so Claude Desktop usage is not reported: %v\n", plan.Path, err)
		return false, nil
	}
	a.stepf("Desktop", "Claude Desktop usage is counted (telemetry settings in %s)", plan.Path)
	return plan.NeedsWrite(), nil
}

// installCodexDesktop asks to count Codex desktop app usage unless the user
// already opted in. Nothing in Codex changes.
func (a *App) installCodexDesktop(p *prompter, paths config.Paths, cfg *config.Config) error {
	if !cfg.DesktopCodexSince.IsZero() {
		a.stepf("Desktop", "Codex desktop app usage is counted")
		return nil
	}
	ok, err := p.confirm("Count Codex desktop app usage? Nothing in Codex changes.")
	if err != nil {
		return err
	}
	if !ok {
		a.stepf("Desktop", "Codex desktop app usage is not counted")
		return nil
	}
	if err := setDesktopSourceQuiet(cfg, paths, "codex", true); err != nil {
		return err
	}
	a.stepf("Desktop", "Codex desktop app usage is counted")
	return nil
}

func persistDesktopPause(dir string, paused, enabled bool) error {
	// An opt-in before the first bind still needs a durable pause window.
	// Without desktop opt-in, PersistPause is a no-op for absent state.
	if paused && enabled {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return errors.New("could not create desktop state for reporting pause")
		}
	}
	return desktop.PersistPause(dir, paused)
}

func (a *App) printClaudeDesktopEnv(paths config.Paths, cfg config.Config) error {
	if cfg.DesktopClaudeSince.IsZero() {
		return errors.New("Claude Desktop collection is off; run `munch desktop claude on` first")
	}
	_, port, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil || port == "0" {
		return errors.New("Claude Desktop collection requires a fixed nonzero Munch listen port")
	}
	if err := desktop.ValidateEndpoint(cfg.IngestURL); err != nil {
		return err
	}
	key, ok := loadLocalKey(paths)
	if !ok {
		return errors.New("no existing Munch local capability; start Munch before printing Claude telemetry settings")
	}
	enc := json.NewEncoder(a.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(claudesettings.Env(claudeEndpoint(cfg.ListenAddr, key)))
}

// desktopRuntime owns the collector from successful bind through HTTP shutdown.
// Calls from HTTP handlers are guarded so a request that outlives the shutdown
// deadline cannot write to a closed ledger.
type desktopRuntime struct {
	dir    string
	paths  config.Paths
	cfg    config.Config
	logger *log.Logger

	workers sync.WaitGroup
	mu      sync.RWMutex
	ledger  *desktop.Collector
	stopped bool

	errMu   sync.Mutex
	lastErr string
}

func (d *desktopRuntime) start(ctx context.Context) {
	if !desktopEnabled(d.cfg) {
		return
	}
	if err := config.ValidateAPIKey(d.cfg.APIKey); err != nil {
		d.report(errors.New("collection inactive: configure a valid TokenMunchers key with `munch login`, then restart Munch"))
		return
	}
	ledger, err := desktop.Open(desktop.Options{
		Dir: d.dir, IngestURL: d.cfg.IngestURL, APIKey: d.cfg.APIKey,
		ClaudeSince: d.cfg.DesktopClaudeSince, CodexSince: d.cfg.DesktopCodexSince,
		Paused: d.cfg.Paused,
	})
	if err != nil {
		d.report(fmt.Errorf("collection inactive: %w; fix desktop state or configuration and restart Munch", err))
		return
	}
	d.ledger = ledger
	d.workers.Add(1)
	go func() {
		defer d.workers.Done()
		ledger.Run(ctx, d.report)
	}()
	if !d.cfg.DesktopCodexSince.IsZero() {
		home, err := desktop.CodexHome()
		if err != nil {
			d.report(fmt.Errorf("Codex capture inactive: %w", err))
			return
		}
		d.workers.Add(1)
		go func() {
			defer d.workers.Done()
			desktop.ScanCodex(ctx, home, ledger.Record, d.report)
		}()
	}
}

// Only fixed diagnostics produced by the desktop package or this integration
// reach the log and authenticated status.
func (d *desktopRuntime) report(err error) {
	if err == nil {
		return
	}
	d.errMu.Lock()
	defer d.errMu.Unlock()
	if msg := err.Error(); msg != d.lastErr {
		d.lastErr = msg
		d.logger.Printf("desktop: %s", msg)
	}
}

func (d *desktopRuntime) stop() {
	d.workers.Wait()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopped = true
	if d.ledger != nil {
		d.report(d.ledger.Close())
		d.ledger = nil
	}
}

func (d *desktopRuntime) claudeLogs() http.Handler {
	if d.cfg.DesktopClaudeSince.IsZero() {
		return nil
	}
	handler := desktop.ClaudeHandler(func(observation desktop.Observation) error {
		d.mu.RLock()
		defer d.mu.RUnlock()
		if d.ledger == nil {
			return errors.New("desktop collection is inactive")
		}
		err := d.ledger.Record(observation)
		d.report(err)
		return err
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.RLock()
		ready := d.ledger != nil
		d.mu.RUnlock()
		if !ready {
			http.Error(w, "desktop collection is inactive", http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(w, r)
	})
}

func (d *desktopRuntime) setPaused(paused bool) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.stopped {
		return errors.New("desktop collection is shutting down")
	}
	if d.ledger != nil {
		err := d.ledger.SetPaused(paused)
		d.report(err)
		return err
	}
	enabled := false
	if paused {
		cfg, err := config.LoadFile(d.paths)
		if err != nil {
			return errors.New("could not read desktop opt-in for reporting pause")
		}
		enabled = desktopEnabled(cfg)
	}
	err := persistDesktopPause(d.dir, paused, enabled)
	d.report(err)
	return err
}

func (d *desktopRuntime) status() *proxy.DesktopStatus {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if !desktopEnabled(d.cfg) {
		return nil
	}
	d.errMu.Lock()
	status := &proxy.DesktopStatus{Error: d.lastErr}
	d.errMu.Unlock()
	if d.ledger != nil {
		pending, delivered := d.ledger.Pending(), d.ledger.Delivered()
		status.Pending, status.Delivered = &pending, &delivered
	}
	return status
}
