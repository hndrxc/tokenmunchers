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
	"strings"
	"sync"
	"time"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/config"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/desktop"
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
		fmt.Fprintln(a.Stdout, "Use `munch desktop claude on` or `munch desktop codex on` to opt in, then restart Munch. Only live usage is collected.")
		fmt.Fprintln(a.Stdout, "For Claude, run `munch desktop claude-env`, privately merge the printed keys into your existing Claude user settings env, and restart Claude Desktop.")
		fmt.Fprintln(a.Stdout, "Use `off` to opt out, restart Munch, and for Claude remove the telemetry keys you added and restart Claude Desktop.")
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

func (a *App) setDesktopSource(paths config.Paths, cfg config.Config, source string, on bool) error {
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
	fmt.Fprintf(a.Stdout, "%s desktop collection configured %s. Restart Munch (`munch restart`).\n", source, desktopState(*since))
	if source == "claude" {
		if on {
			fmt.Fprintln(a.Stdout, "Run `munch desktop claude-env`, privately merge the printed keys into your existing Claude user settings env, and restart Claude Desktop.")
		} else {
			fmt.Fprintln(a.Stdout, "Remove the telemetry keys you added to Claude user settings env and restart Claude Desktop.")
		}
	}
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
	return enc.Encode(map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY":             "1",
		"OTEL_LOGS_EXPORTER":                       "otlp",
		"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL":         "http/json",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":         "http://" + cfg.ListenAddr + key.RoutePrefix() + proxy.ClaudeLogsPath,
		"OTEL_LOGS_EXPORT_INTERVAL":                "1000",
		"OTEL_METRICS_EXPORTER":                    "none",
		"OTEL_TRACES_EXPORTER":                     "none",
		"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA":      "0",
		"OTEL_METRICS_INCLUDE_ENTRYPOINT":          "true",
		"OTEL_METRICS_INCLUDE_SESSION_ID":          "false",
		"OTEL_METRICS_INCLUDE_ACCOUNT_UUID":        "false",
		"OTEL_METRICS_INCLUDE_RESOURCE_ATTRIBUTES": "false",
		"OTEL_RESOURCE_INCLUDE_HOST":               "0",
		"OTEL_LOG_USER_PROMPTS":                    "0",
		"OTEL_LOG_ASSISTANT_RESPONSES":             "0",
		"OTEL_LOG_TOOL_DETAILS":                    "0",
		"OTEL_LOG_TOOL_CONTENT":                    "0",
		"OTEL_LOG_MANAGED_SETTINGS":                "0",
		"OTEL_LOG_RAW_API_BODIES":                  "0",
	})
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
