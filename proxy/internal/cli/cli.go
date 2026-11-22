package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/config"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/ingest"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/localauth"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/localstore"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/omp"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/pricing"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/proxy"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/service"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/shellrc"
)

// App holds I/O streams for testability.
type App struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// newService replaces service.NewManager for install and uninstall, so
	// tests never touch the real service manager.
	newService func(paths config.Paths, listenAddr string) service.Manager
}

// DefaultApp returns an App wired to standard OS streams.
func DefaultApp() *App {
	return &App{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
}

// serviceManager returns the OS service manager for paths and listenAddr.
func (a *App) serviceManager(paths config.Paths, listenAddr string) service.Manager {
	if a.newService != nil {
		return a.newService(paths, listenAddr)
	}
	return service.NewManager(paths, listenAddr)
}

// Run dispatches CLI subcommands and returns an exit code.
func (a *App) Run(args []string) int {
	if len(args) == 0 {
		a.printUsage()
		return 0
	}

	cmd := args[0]
	rest := args[1:]

	var err error
	switch cmd {
	case "help", "-h", "--help":
		a.printUsage()
		return 0
	case "version", "-v", "--version":
		fmt.Fprintf(a.Stdout, "munch %s (%s/%s)\n", config.Version, runtime.GOOS, runtime.GOARCH)
		return 0
	case "login":
		err = a.cmdLogin(rest)
	case "run":
		err = a.cmdRun(rest)
	case "start":
		err = a.cmdStart(rest)
	case "stop":
		err = a.cmdStop(rest)
	case "restart":
		err = a.cmdRestart(rest)
	case "install":
		err = a.cmdInstall(rest)
	case "update":
		err = a.cmdUpdate(rest)
	case "uninstall":
		err = a.cmdUninstall(rest)
	case "status":
		err = a.cmdStatus(rest)
	case "pause":
		err = a.cmdPause(rest, true)
	case "resume":
		err = a.cmdPause(rest, false)
	case "flush":
		err = a.cmdFlush(rest)
	case "backfill":
		err = a.cmdBackfill(rest)
	case "env":
		err = a.cmdEnv(rest)
	case "exec":
		return a.cmdExec(rest)
	case "stats":
		err = a.cmdStats(rest)
	case "routes":
		err = a.cmdRoutes(rest)
	default:
		fmt.Fprintf(a.Stderr, "unknown command %q\n\n", cmd)
		a.printUsage()
		return 2
	}

	if err != nil {
		fmt.Fprintf(a.Stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func (a *App) printUsage() {
	fmt.Fprintf(a.Stdout, `munch %s — local inference proxy for TokenMunchers

Usage:
  munch <command> [flags]

Commands:
  login [tm_key]       Save your TokenMunchers API key (create one in Dashboard Settings)
  run                  Run the proxy in the foreground (used by systemd/launchd)
  start                Start the background proxy daemon
  stop                 Stop the background proxy daemon
  restart              Restart the background proxy daemon
  install              Set up the background service, and offer to configure OMP, the shell,
                       and Codex with ChatGPT sign-in (--yes, --no-shell, --no-codex)
  update               Replace this binary with the latest stable Munch release
  uninstall            Stop and remove the service and remove Munch-managed shell and Codex setup
  status               Show proxy runtime, queue, and service status
  pause                Pause reporting (proxied AI traffic continues uninterrupted)
  resume               Resume reporting
  flush                Immediately flush queued usage events to TokenMunchers
  env                  Print shell exports (*_BASE_URL) to route tools through munch.
                       By default, exports require an authenticated running proxy.
                       The exports contain a private capability URL; do not share or log them.
                       When Oh My Pi is installed or omp_via_plugin is set, also defines an omp
                       function that runs it without OLLAMA_HOST (OMP keeps only host:port of it,
                       so the private route is lost), or without any of these variables when
                       omp_via_plugin is set (omp-tm reports its usage instead).
  exec -- <cmd...>     Run a single command with munch env vars injected (requires a running proxy);
                       omp gets the same variables its shell wrapper leaves out
  stats                Show local captured token totals (today, week, all)
  routes               List configured provider routes (capability redacted; --show-local-urls reveals it)
  backfill             Preview or upload retained Codex history (--from/--before required to upload)
  version              Print version info

Run "munch <command> -h" for flags on any command.
`, config.Version)
}

func addPathFlags(fs *flag.FlagSet) (*string, *string) {
	cfgDir := fs.String("config-dir", "", "override config directory")
	stateDir := fs.String("state-dir", "", "override state directory")
	return cfgDir, stateDir
}

func resolvePaths(cfgDir, stateDir string) (config.Paths, error) {
	paths, err := config.DefaultPaths()
	if err != nil {
		return config.Paths{}, err
	}
	if cfgDir != "" {
		paths.ConfigDir = cfgDir
	}
	if stateDir != "" {
		paths.StateDir = stateDir
	}
	return paths, nil
}

// errNoKeyEntered reports an empty answer, or EOF, at the API key prompt.
var errNoKeyEntered = errors.New("no API key entered")

// prompter reads interactive answers from one buffered reader, so every prompt
// in a command consumes the same input.
type prompter struct {
	in  *bufio.Reader
	out io.Writer
	yes bool // answer confirmations with their default instead of asking
}

func newPrompter(in io.Reader, out io.Writer, yes bool) *prompter {
	return &prompter{in: bufio.NewReader(in), out: out, yes: yes}
}

// apiKey asks for a TokenMunchers API key and checks its format. An empty
// answer, or EOF, yields errNoKeyEntered.
func (p *prompter) apiKey() (string, error) {
	fmt.Fprint(p.out, "Paste your TokenMunchers API key (tm_...): ")
	line, err := p.in.ReadString('\n')
	if errors.Is(err, io.EOF) {
		fmt.Fprintln(p.out) // input ended mid-line; finish the prompt line
	} else if err != nil {
		return "", err
	}
	key := strings.TrimSpace(line)
	if key == "" {
		return "", errNoKeyEntered
	}
	if err := config.ValidateAPIKey(key); err != nil {
		return "", err
	}
	return key, nil
}

// confirm asks a yes/no question whose default is yes: an empty answer, "y" or
// "yes" is yes, "n" or "no" is no, and anything else asks again. At EOF it
// takes the default. With p.yes set it answers without reading input.
func (p *prompter) confirm(question string) (bool, error) {
	if p.yes {
		fmt.Fprintf(p.out, "%s [Y/n] y\n", question)
		return true, nil
	}
	return p.askConfirmation(question, true)
}

func (p *prompter) askConfirmation(question string, defaultYes bool) (bool, error) {
	suffix := "[y/N]"
	if defaultYes {
		suffix = "[Y/n]"
	}
	for {
		fmt.Fprintf(p.out, "%s %s ", question, suffix)
		line, err := p.in.ReadString('\n')
		eof := errors.Is(err, io.EOF)
		if err != nil && !eof {
			return false, err
		}
		if eof {
			fmt.Fprintln(p.out) // input ended mid-line; finish the prompt line
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		case "":
			return defaultYes, nil
		}
		if eof {
			return defaultYes, nil
		}
		fmt.Fprintln(p.out, "Please answer y or n.")
	}
}

func (a *App) cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	cfgDir, stateDir := addPathFlags(fs)
	ingestURL := fs.String("url", "", "custom TokenMunchers ingest URL")
	listen := fs.String("listen", "", "proxy listen address (default 127.0.0.1:8787)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	paths, err := resolvePaths(*cfgDir, *stateDir)
	if err != nil {
		return err
	}

	var key string
	if fs.NArg() > 0 {
		key = strings.TrimSpace(fs.Arg(0))
		if err := config.ValidateAPIKey(key); err != nil {
			return err
		}
	} else if key, err = newPrompter(a.Stdin, a.Stdout, false).apiKey(); err != nil {
		return err
	}

	diskCfg, err := config.LoadFile(paths)
	if err != nil {
		return err
	}
	diskCfg.APIKey = key
	if *ingestURL != "" {
		u := strings.TrimSpace(*ingestURL)
		if err := ingest.ValidateURL(u); err != nil {
			return err
		}
		diskCfg.IngestURL = u
	}
	if *listen != "" {
		addr, err := config.NormalizeListenAddr(strings.TrimSpace(*listen))
		if err != nil {
			return err
		}
		diskCfg.ListenAddr = addr
	}
	if err := config.Save(paths, diskCfg); err != nil {
		return err
	}

	fmt.Fprintf(a.Stdout, "Saved API key (%s) to %s\n", configuredLabel(key), paths.ConfigFile())
	fmt.Fprintln(a.Stdout, "Next: run `munch install` to start the proxy and route your tools through it.")
	return nil
}

func (a *App) cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	cfgDir, stateDir := addPathFlags(fs)
	listen := fs.String("listen", "", "override listen address (default 127.0.0.1:8787)")
	verbose := fs.Bool("verbose", false, "log captured metadata per call")
	if err := fs.Parse(args); err != nil {
		return err
	}

	paths, cfg, err := loadPathsAndConfig(*cfgDir, *stateDir, *listen)
	if err != nil {
		return err
	}
	if err := ingest.ValidateURL(cfg.IngestURL); err != nil {
		return err
	}

	priceTable, err := pricing.LoadTable(paths.PricesFile())
	if err != nil {
		fmt.Fprintf(a.Stderr, "warning: failed loading %s (%v); using built-in pricing\n", paths.PricesFile(), err)
		priceTable = pricing.NewTable(nil)
	}

	localKey, err := localauth.LoadOrCreate(paths.ConfigDir)
	if err != nil {
		return err
	}

	reporter := ingest.NewReporter(ingest.Options{
		QueuePath: paths.QueueFile(),
		IngestURL: cfg.IngestURL,
		APIKey:    cfg.APIKey,
	})

	store := localstore.New(paths.UsageLogFile())
	logger := log.New(a.Stderr, "munch: ", log.LstdFlags)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// The reporter only runs once the listener is bound, so a failed startup
	// (e.g. address in use) never touches the network or the on-disk queue.
	var reporterStarted atomic.Bool
	srv, err := proxy.New(proxy.Options{
		Config:        cfg,
		Paths:         paths,
		Pricing:       priceTable,
		Reporter:      reporter,
		LocalStore:    store,
		Logger:        logger,
		Verbose:       *verbose,
		OnShutdownReq: cancel,
		LocalKey:      localKey,
		OnReady: func() {
			reporter.Start()
			reporterStarted.Store(true)
			logger.Printf("listening on http://%s (paused=%v)", cfg.ListenAddr, cfg.Paused)
		},
	})
	if err != nil {
		return err
	}

	runErr := srv.ListenAndServe(ctx)

	if reporterStarted.Load() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		reporter.Stop(stopCtx)
		stopCancel()
	}

	return runErr
}

func (a *App) cmdStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	cfgDir, stateDir := addPathFlags(fs)
	listen := fs.String("listen", "", "override listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}

	paths, cfg, err := loadPathsAndConfig(*cfgDir, *stateDir, *listen)
	if err != nil {
		return err
	}
	bin, err := executablePath()
	if err != nil {
		return err
	}
	mgr := service.NewManager(paths, cfg.ListenAddr)
	if err := mgr.Start(bin); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "munch started on http://%s (%s)\n", cfg.ListenAddr, mgr.Status().Backend)
	return nil
}

func (a *App) cmdStop(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	cfgDir, stateDir := addPathFlags(fs)
	listen := fs.String("listen", "", "override listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}

	paths, cfg, err := loadPathsAndConfig(*cfgDir, *stateDir, *listen)
	if err != nil {
		return err
	}
	mgr := service.NewManager(paths, cfg.ListenAddr)
	if err := mgr.Stop(); err != nil {
		return err
	}
	fmt.Fprintln(a.Stdout, "munch stopped")
	return nil
}

func (a *App) cmdRestart(args []string) error {
	if err := a.cmdStop(args); err != nil {
		return err
	}
	return a.cmdStart(args)
}

// installHealthTimeout is how long install waits for the daemon to answer. It
// is a variable so tests can shorten it.
var installHealthTimeout = 10 * time.Second

// errNoKeyAvailable is returned when install has no API key and cannot ask for one.
var errNoKeyAvailable = errors.New("no API key: run `munch login` (or set TOKENMUNCHERS_KEY), then run `munch install` again")

// cmdInstall sets munch up end to end: API key, background service, health
// check, the omp-tm choice for Oh My Pi, the shell profile hook, and Codex
// signed in with ChatGPT.
func (a *App) cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	cfgDir, stateDir := addPathFlags(fs)
	listen := fs.String("listen", "", "override listen address")
	yes := fs.Bool("yes", false, "don't prompt; accept the recommended answer to every question")
	noShell := fs.Bool("no-shell", false, "don't modify shell profiles")
	noCodex := fs.Bool("no-codex", false, "don't inspect or modify Codex settings")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("install takes no arguments; save your API key with `munch login` or set TOKENMUNCHERS_KEY")
	}

	paths, cfg, err := loadPathsAndConfig(*cfgDir, *stateDir, *listen)
	if err != nil {
		return err
	}
	bin, err := executablePath()
	if err != nil {
		return err
	}
	p := newPrompter(a.Stdin, a.Stdout, *yes)

	key, err := a.installAPIKey(p, paths, cfg.APIKey)
	if err != nil {
		return err
	}
	svc, err := a.installService(paths, cfg.ListenAddr, bin)
	if err != nil {
		return err
	}
	if err := a.installOMP(p, paths, &cfg, key); err != nil {
		return err
	}
	flags := installEnvFlags(*cfgDir, *stateDir, *listen)
	sh, err := a.installShell(p, bin, *noShell, flags)
	if err != nil {
		return err
	}
	cx, err := a.installCodex(p, paths, cfg, *noCodex)
	if err != nil {
		return err
	}
	a.printInstallSummary(cfg, svc, sh, cx, flags)
	return nil
}

// stepf prints one progress line of install under label.
func (a *App) stepf(label, format string, args ...any) {
	fmt.Fprintf(a.Stdout, "%-8s %s\n", label+":", fmt.Sprintf(format, args...))
}

// installAPIKey makes sure the API key is saved in config.json and returns it.
// The background service does not inherit your shell environment, so a key
// that only exists in TOKENMUNCHERS_KEY is written to disk too. The key is
// never printed.
func (a *App) installAPIKey(p *prompter, paths config.Paths, envKey string) (string, error) {
	disk, err := config.LoadFile(paths)
	if err != nil {
		return "", err
	}
	if disk.APIKey != "" {
		a.stepf("API key", "already saved in %s", paths.ConfigFile())
		return disk.APIKey, nil
	}

	key, src := envKey, ""
	switch {
	case key != "":
		src = envAPIKeySource()
		if err := config.ValidateAPIKey(key); err != nil {
			return "", fmt.Errorf("%s: %w", src, err)
		}
	case p.yes:
		return "", errNoKeyAvailable
	default:
		key, err = p.apiKey()
		if errors.Is(err, errNoKeyEntered) {
			return "", errNoKeyAvailable
		}
		if err != nil {
			return "", err
		}
	}

	disk.APIKey = key
	if err := config.Save(paths, disk); err != nil {
		return "", err
	}
	if src != "" {
		a.stepf("API key", "using $%s, saved to %s (the background service can't see your shell environment)", src, paths.ConfigFile())
	} else {
		a.stepf("API key", "saved to %s", paths.ConfigFile())
	}
	return key, nil
}

// envAPIKeySource names the environment variable config.Load takes the API key
// from. Keep the order in sync with config.Load.
func envAPIKeySource() string {
	for _, name := range []string{"TOKENMUNCHERS_KEY", "TOKENMUNCHERS_API_KEY"} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return name
		}
	}
	return ""
}

// serviceOutcome says how the proxy is running after install.
type serviceOutcome struct {
	backend   string // "systemd-user", "launchd" or "pidfile"
	autoStart bool   // the OS starts it again after login or reboot
}

// installService (re)installs the background service and waits until the daemon
// proves it holds this config's local key.
func (a *App) installService(paths config.Paths, listen, bin string) (serviceOutcome, error) {
	localKey, err := localauth.LoadOrCreate(paths.ConfigDir)
	if err != nil {
		return serviceOutcome{}, err
	}
	mgr := a.serviceManager(paths, listen)
	out := serviceOutcome{backend: mgr.Backend()}

	// A running daemon keeps its old binary, config and key until it restarts.
	if mgr.Status().Running {
		a.stepf("Service", "stopping the running munch so the new setup takes effect")
		if err := mgr.Stop(); err != nil {
			return out, fmt.Errorf("stop running munch: %w", err)
		}
	}

	unit, err := mgr.Install(bin)
	switch {
	case err != nil && out.backend == "pidfile":
		a.stepf("Service", "auto-start is not available on this system; munch will run in the background until you stop it or restart")
	case err != nil:
		return out, fmt.Errorf("install %s service: %w", out.backend, err)
	case out.backend == "pidfile":
		// Linux without a systemd user session: the unit file was written, but
		// nothing will run it.
		a.stepf("Service", "systemd --user is not available, so %s will not start munch; it will run in the background until you stop it or restart", unit)
	default:
		out.autoStart = true
		a.stepf("Service", "installed %s service (%s)", out.backend, unit)
	}

	// Install may already have started the service, so a Start error only
	// matters if the daemon never comes up.
	startErr := mgr.Start(bin)
	if !waitHealthy(listen, localKey, installHealthTimeout) {
		if startErr != nil {
			return out, fmt.Errorf("start munch: %w%s", startErr, startupHint(out.backend, paths))
		}
		return out, fmt.Errorf("munch did not answer on http://%s within %s%s", listen, installHealthTimeout, startupHint(out.backend, paths))
	}
	a.stepf("Service", "munch is running on http://%s", listen)
	return out, nil
}

// waitHealthy polls the daemon on listen until it proves it holds key, or
// timeout passes. It probes at least once.
func waitHealthy(listen string, key localauth.Key, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if service.ProbeHealth(context.Background(), listen, key) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// startupHint tells the user where to look when the daemon did not come up.
func startupHint(backend string, paths config.Paths) string {
	hint := "; run `munch status` to investigate"
	if backend == "systemd-user" {
		return hint + " or `journalctl --user -u tmproxy` for the service log"
	}
	return hint + ", or see " + paths.LogFile()
}

// installOMP offers omp-tm when Oh My Pi is on PATH and records the answer as
// omp_via_plugin. Trouble with omp or its plugin is only a warning: the proxy
// is already running and OMP keeps its current routing.
func (a *App) installOMP(p *prompter, paths config.Paths, cfg *config.Config, key string) error {
	ompBin, ok := omp.Find()
	if !ok {
		return nil
	}

	checkCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	installed, err := omp.PluginInstalled(checkCtx, ompBin)
	cancel()
	if err != nil {
		fmt.Fprintf(a.Stderr, "warning: could not tell whether the %s plugin is installed: %v\n", omp.PluginName, err)
		installed = false
	}

	useTM, err := p.confirm("Oh My Pi detected. Report OMP usage with omp-tm (the omp-tokenmunchers plugin) instead of the proxy, so calls are not counted twice?")
	if err != nil {
		return err
	}
	if !useTM {
		if err := setOMPViaPlugin(paths, cfg, false); err != nil {
			return err
		}
		a.stepf("OMP", "omp-tm not enabled")
		if installed {
			fmt.Fprintf(a.Stderr, "warning: the %s plugin is installed, so OMP calls will be counted twice (by the plugin and the proxy); turn it off with `omp plugin disable %s`\n", omp.PluginName, omp.PluginName)
		}
		return nil
	}

	if err := a.enableOMPPlugin(ompBin, installed, key, cfg.IngestURL); err != nil {
		fmt.Fprintf(a.Stderr, "warning: omp-tm is not set up: %v\n", err)
		fmt.Fprintln(a.Stderr, "OMP routing is unchanged; fix the problem and run `munch install` again.")
		return nil
	}
	if err := setOMPViaPlugin(paths, cfg, true); err != nil {
		return err
	}
	a.stepf("OMP", "OMP usage is reported by omp-tm and kept out of the proxy")
	return nil
}

// enableOMPPlugin installs the omp-tokenmunchers plugin if it is missing and
// gives it the API key unless it already has one.
func (a *App) enableOMPPlugin(ompBin string, installed bool, key, ingestURL string) error {
	if !installed {
		a.stepf("OMP", "installing the %s plugin", omp.PluginName)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := omp.InstallPlugin(ctx, ompBin, a.Stdout, a.Stderr); err != nil {
			return err
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("find your home directory: %w", err)
	}
	if ingestURL == config.DefaultIngestURL {
		ingestURL = "" // the plugin's own default
	}
	seeded, err := omp.SeedKey(omp.AgentDir(os.Getenv, home), key, ingestURL)
	if err != nil {
		return fmt.Errorf("configure the plugin: %w", err)
	}
	if seeded {
		a.stepf("OMP", "gave omp-tm your API key")
	} else {
		a.stepf("OMP", "omp-tm already has an API key; left unchanged")
	}
	return nil
}

// setOMPViaPlugin records in config.json whether OMP reports through omp-tm.
// It edits what is stored on disk, never the environment-merged config.
func setOMPViaPlugin(paths config.Paths, cfg *config.Config, on bool) error {
	if cfg.OMPViaPlugin == on {
		return nil
	}
	disk, err := config.LoadFile(paths)
	if err != nil {
		return err
	}
	disk.OMPViaPlugin = on
	if err := config.Save(paths, disk); err != nil {
		return err
	}
	cfg.OMPViaPlugin = on
	return nil
}

// envFlag is a command-line flag `munch env` must repeat to find the proxy
// that install set up.
type envFlag struct{ name, value string }

// installEnvFlags returns the install flags that change where a flag-less
// `munch env --check` looks for the proxy. The managed profile block runs
// that flag-less command, so these cannot be carried by it.
func installEnvFlags(cfgDir, stateDir, listen string) []envFlag {
	var flags []envFlag
	if cfgDir != "" {
		flags = append(flags, envFlag{"--config-dir", absPath(cfgDir)})
	}
	if stateDir != "" {
		flags = append(flags, envFlag{"--state-dir", absPath(stateDir)})
	}
	if v := strings.TrimSpace(listen); v != "" {
		flags = append(flags, envFlag{"--listen", v})
	}
	return flags
}

// absPath makes p independent of the working directory, which a profile line
// cannot rely on.
func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// shellOutcome says how new shells get the proxy environment after install.
type shellOutcome struct {
	wired string // profile that routes new shells through the proxy; empty if none does
	where string // where to add the line yourself: the detected profile, if any
}

// installShell adds the managed block to the user's shell profile. Anything
// that keeps it from doing so is reported and leaves wired empty.
func (a *App) installShell(p *prompter, bin string, noShell bool, flags []envFlag) (shellOutcome, error) {
	var out shellOutcome
	home, homeErr := os.UserHomeDir()
	var profile shellrc.Profile
	detected := false
	if homeErr == nil {
		profile, detected = shellrc.Detect(os.Getenv, home, runtime.GOOS)
	}
	if detected {
		out.where = profile.Path
	}

	switch {
	case noShell:
		a.stepf("Shell", "profile not modified (--no-shell)")
		return out, nil
	case len(flags) > 0:
		names := make([]string, len(flags))
		for i, f := range flags {
			names[i] = f.name
		}
		a.stepf("Shell", "profile not modified: the block munch adds runs `munch env --check` without %s, so it would not find this proxy", strings.Join(names, ", "))
		return out, nil
	case homeErr != nil:
		fmt.Fprintf(a.Stderr, "warning: cannot find your home directory, so no shell profile was changed: %v\n", homeErr)
		return out, nil
	case !detected:
		a.stepf("Shell", "profile not modified: munch only edits bash, zsh and fish profiles (%s)", shellNote())
		return out, nil
	}

	state, err := shellrc.Inspect(profile)
	if err != nil {
		fmt.Fprintf(a.Stderr, "warning: cannot read %s: %v\n", profile.Path, err)
		a.stepf("Shell", "profile not modified")
		return out, nil
	}
	if state.Unmanaged && !state.Managed {
		a.stepf("Shell", "already wired: a line in %s runs `munch env`", profile.Path)
		out.wired = profile.Path
		return out, nil
	}
	if state.Unmanaged {
		fmt.Fprintf(a.Stderr, "warning: %s also runs `munch env` from a line outside munch's block; you can delete that line\n", profile.Path)
	}

	question := fmt.Sprintf("Add munch to %s so new shells route AI tools through the proxy?", profile.Path)
	if state.Managed {
		question = fmt.Sprintf("Update the munch block in %s?", profile.Path)
	}
	ok, err := p.confirm(question)
	if err != nil {
		return out, err
	}
	if !ok {
		if state.Managed {
			out.wired = profile.Path
		}
		a.stepf("Shell", "profile left as is")
		return out, nil
	}

	changed, err := shellrc.Install(profile, bin)
	if err != nil {
		fmt.Fprintf(a.Stderr, "warning: could not update %s: %v\n", profile.Path, err)
		return out, nil
	}
	if changed {
		a.stepf("Shell", "updated %s", profile.Path)
	} else {
		a.stepf("Shell", "%s is already up to date", profile.Path)
	}
	out.wired = profile.Path
	return out, nil
}

// shellNote describes the user's shell for messages.
func shellNote() string {
	s := os.Getenv("SHELL")
	if s == "" {
		return "SHELL is not set"
	}
	return "your shell is " + filepath.Base(s)
}

// printInstallSummary closes the install output with what is active and what
// is left for the user to do.
func (a *App) printInstallSummary(cfg config.Config, svc serviceOutcome, sh shellOutcome, cx codexOutcome, flags []envFlag) {
	fmt.Fprintf(a.Stdout, "\nmunch is running on http://%s (%s).\n", cfg.ListenAddr, svc.backend)
	if !svc.autoStart {
		fmt.Fprintln(a.Stdout, "It does not start by itself after a reboot or logout; run `munch start` then.")
	}
	kind := detectShell()
	if sh.wired != "" {
		fmt.Fprintf(a.Stdout, "New shells route Claude Code, Codex, Gemini and other AI tools through it (set up in %s).\n", sh.wired)
	} else {
		where := sh.where
		if where == "" {
			where = "your shell profile"
			if kind == "powershell" {
				where = "your PowerShell profile ($PROFILE)"
			}
		}
		fmt.Fprintf(a.Stdout, "To route AI tools through it, add this line to %s:\n\n  %s\n\n", where, envHookLine(kind, flags))
	}
	if cfg.OMPViaPlugin {
		if sh.wired != "" {
			fmt.Fprintln(a.Stdout, "OMP reports through omp-tm, and new shells run `omp` without the proxy variables, so its calls are counted once.")
		} else {
			fmt.Fprintln(a.Stdout, "OMP reports through omp-tm, and that line also defines an `omp` function that runs it without the proxy variables, so its calls are counted once.")
		}
	} else if ompOnPath() {
		// OMP keeps only host:port of OLLAMA_HOST, so the private route would
		// be lost and its Ollama discovery would fail against the proxy root.
		if sh.wired != "" {
			fmt.Fprintln(a.Stdout, "OMP is proxied, but gets no OLLAMA_HOST: it would drop the private route, so it talks to local Ollama directly.")
		} else {
			fmt.Fprintln(a.Stdout, "OMP is proxied, but that line also defines an `omp` function that leaves out OLLAMA_HOST: OMP would drop the private route, so it talks to local Ollama directly.")
		}
	}
	switch {
	case cx.routed != "":
		fmt.Fprintf(a.Stdout, "Codex signed in with ChatGPT is routed through it too (Munch provider in %s); start a new Codex process to pick that up.\n", cx.routed)
	case cx.unrouted:
		fmt.Fprintln(a.Stdout, "Codex signed in with ChatGPT is not routed through it; run `munch install` again to set that up, or follow the manual Codex steps in the proxy README.")
	}
	if sh.wired != "" {
		fmt.Fprintln(a.Stdout, "Open a new terminal to pick this up: shells and apps that are already running keep their old environment.")
	} else {
		fmt.Fprintln(a.Stdout, "Once that line is in place, open a new terminal: shells and apps that are already running keep their old environment.")
	}
	fmt.Fprintln(a.Stdout, "Check on it any time with `munch status`.")
}

func (a *App) cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	cfgDir, stateDir := addPathFlags(fs)
	listen := fs.String("listen", "", "override listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}

	paths, cfg, err := loadPathsAndConfig(*cfgDir, *stateDir, *listen)
	if err != nil {
		return err
	}
	mgr := a.serviceManager(paths, cfg.ListenAddr)
	if err := mgr.Uninstall(); err != nil {
		return err
	}
	fmt.Fprintln(a.Stdout, "Uninstalled munch service")
	hookErr := a.removeShellHooks()
	codexErr := a.removeCodexRoute()
	if cfg.OMPViaPlugin {
		fmt.Fprintf(a.Stdout, "The %s plugin was left installed and keeps reporting OMP usage; remove it with `omp plugin uninstall %s` if you don't want that.\n", omp.PluginName, omp.PluginName)
	}
	return errors.Join(hookErr, codexErr)
}

// removeShellHooks deletes the munch block from every profile install may
// have written, and points out hooks the user added by hand, which it cannot
// safely remove.
func (a *App) removeShellHooks() error {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(a.Stderr, "warning: cannot find your home directory, so shell profiles were not checked: %v\n", err)
		return nil
	}
	var errs []error
	removed := false
	for _, profile := range shellrc.Candidates(os.Getenv, home, runtime.GOOS) {
		changed, err := shellrc.Remove(profile)
		if err != nil {
			errs = append(errs, fmt.Errorf("remove munch from %s: %w", profile.Path, err))
			continue
		}
		if changed {
			removed = true
			fmt.Fprintf(a.Stdout, "Removed munch from %s\n", profile.Path)
		}
		if state, err := shellrc.Inspect(profile); err == nil && state.Unmanaged {
			fmt.Fprintf(a.Stdout, "%s still runs `munch env` from a line you added; remove that line manually.\n", profile.Path)
		}
	}
	if removed {
		fmt.Fprintln(a.Stdout, "Shells that are already open keep their proxy variables; open a new terminal, or run `eval \"$(munch env --unset)\"` there.")
	}
	return errors.Join(errs...)
}

func (a *App) cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	cfgDir, stateDir := addPathFlags(fs)
	listen := fs.String("listen", "", "override listen address")
	asJSON := fs.Bool("json", false, "output status as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	paths, cfg, err := loadPathsAndConfig(*cfgDir, *stateDir, *listen)
	if err != nil {
		return err
	}
	mgr := service.NewManager(paths, cfg.ListenAddr)
	svcInfo := mgr.Status()

	var live *proxy.StatusSnapshot
	if key, ok := loadLocalKey(paths); ok {
		body, code, err := service.GetControl(context.Background(), cfg.ListenAddr, "status", key)
		if err == nil && code == 200 {
			var snap proxy.StatusSnapshot
			if json.Unmarshal(body, &snap) == nil {
				live = &snap
			}
		}
	}

	if *asJSON {
		out := map[string]any{
			"service": svcInfo,
			"running": live != nil,
			"live":    live,
		}
		enc := json.NewEncoder(a.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if live != nil {
		state := "reporting"
		if live.Paused {
			state = "paused"
		}
		fmt.Fprintf(a.Stdout, "Status:       running (%s)\n", state)
		fmt.Fprintf(a.Stdout, "PID:          %d\n", live.PID)
		fmt.Fprintf(a.Stdout, "Listen:       http://%s\n", live.ListenAddr)
		fmt.Fprintf(a.Stdout, "API Key:      %s\n", configuredLabel(cfg.APIKey))
		fmt.Fprintf(a.Stdout, "Uptime:       %s\n", (time.Duration(live.UptimeSec) * time.Second).String())
		fmt.Fprintf(a.Stdout, "Captured:     %d calls (%d skipped while paused)\n", live.Captured, live.SkippedPause)
		fmt.Fprintf(a.Stdout, "Delivered:    %d events (%d queued, %d dropped)\n", live.Ingest.Delivered, live.Ingest.Queued, live.Ingest.Dropped)
		if live.Ingest.LastError != "" {
			fmt.Fprintf(a.Stdout, "Last Error:   %s\n", live.Ingest.LastError)
		}
	} else {
		rep := ingest.NewReporter(ingest.Options{QueuePath: paths.QueueFile()})
		fmt.Fprintln(a.Stdout, "Status:       stopped")
		fmt.Fprintf(a.Stdout, "Listen:       http://%s\n", cfg.ListenAddr)
		fmt.Fprintf(a.Stdout, "API Key:      %s\n", configuredLabel(cfg.APIKey))
		fmt.Fprintf(a.Stdout, "Queued:       %d events\n", rep.QueueSize())
	}
	fmt.Fprintf(a.Stdout, "Service:      %s (installed=%v)\n", svcInfo.Backend, svcInfo.Installed)
	if svcInfo.UnitPath != "" {
		fmt.Fprintf(a.Stdout, "Unit/PID:     %s\n", svcInfo.UnitPath)
	}
	return nil
}

func (a *App) cmdPause(args []string, pause bool) error {
	name := "resume"
	if pause {
		name = "pause"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	cfgDir, stateDir := addPathFlags(fs)
	listen := fs.String("listen", "", "override listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}

	paths, cfg, err := loadPathsAndConfig(*cfgDir, *stateDir, *listen)
	if err != nil {
		return err
	}

	// Update running proxy if reachable and it proves it holds our local key.
	if key, ok := loadLocalKey(paths); ok && service.ProbeHealth(context.Background(), cfg.ListenAddr, key) {
		_, code, err := service.PostControl(context.Background(), cfg.ListenAddr, name, key)
		if err != nil || code != 200 {
			return fmt.Errorf("failed to %s running proxy: %v (status %d)", name, err, code)
		}
	} else {
		// Persist directly so pause state holds across next start.
		diskCfg, err := config.LoadFile(paths)
		if err != nil {
			return err
		}
		diskCfg.Paused = pause
		if err := config.Save(paths, diskCfg); err != nil {
			return err
		}
	}

	if pause {
		fmt.Fprintln(a.Stdout, "Reporting paused. Proxied requests continue uninterrupted and are not recorded.")
	} else {
		fmt.Fprintln(a.Stdout, "Reporting resumed.")
	}
	return nil
}

func (a *App) cmdFlush(args []string) error {
	fs := flag.NewFlagSet("flush", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	cfgDir, stateDir := addPathFlags(fs)
	listen := fs.String("listen", "", "override listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}

	paths, cfg, err := loadPathsAndConfig(*cfgDir, *stateDir, *listen)
	if err != nil {
		return err
	}

	if key, ok := loadLocalKey(paths); ok && service.ProbeHealth(context.Background(), cfg.ListenAddr, key) {
		body, code, err := service.PostControl(context.Background(), cfg.ListenAddr, "flush", key)
		if err != nil || code != 200 {
			return fmt.Errorf("flush failed: %v (status %d)", err, code)
		}
		var res struct {
			OK    bool         `json:"ok"`
			Error string       `json:"error"`
			Stats ingest.Stats `json:"stats"`
		}
		_ = json.Unmarshal(body, &res)
		if !res.OK {
			return fmt.Errorf("flush error: %s", res.Error)
		}
		fmt.Fprintf(a.Stdout, "Flushed queue (delivered=%d, queued=%d)\n", res.Stats.Delivered, res.Stats.Queued)
		return nil
	}

	// Offline flush directly from disk queue.
	rep := ingest.NewReporter(ingest.Options{
		QueuePath: paths.QueueFile(),
		IngestURL: cfg.IngestURL,
		APIKey:    cfg.APIKey,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := rep.Flush(ctx, true); err != nil {
		return err
	}
	st := rep.Stats()
	fmt.Fprintf(a.Stdout, "Flushed queue (delivered=%d, queued=%d)\n", st.Delivered, st.Queued)
	return nil
}

// EnvPairs returns the provider BASE_URL environment variables for a given proxy base URL.
func EnvPairs(baseURL string) [][2]string {
	base := strings.TrimRight(baseURL, "/")
	return [][2]string{
		{"ANTHROPIC_BASE_URL", base + "/anthropic"},
		{"OPENAI_BASE_URL", base + "/openai/v1"},
		{"GEMINI_API_BASE_URL", base + "/gemini"},
		{"GOOGLE_GEMINI_BASE_URL", base + "/gemini"},
		{"XAI_BASE_URL", base + "/xai/v1"},
		{"GROK_MODELS_BASE_URL", base + "/grok"},
		{"OPENROUTER_BASE_URL", base + "/openrouter/api/v1"},
		{"OLLAMA_HOST", base + "/ollama"},
	}
}

func (a *App) cmdEnv(args []string) error {
	fs := flag.NewFlagSet("env", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	cfgDir, stateDir := addPathFlags(fs)
	listen := fs.String("listen", "", "override listen address")
	shell := fs.String("shell", "", "shell syntax: sh, bash, zsh, fish, powershell (auto-detected)")
	check := fs.Bool("check", true, "require an authenticated running proxy before printing private URLs")
	unset := fs.Bool("unset", false, "print commands to unset proxy env vars")
	if err := fs.Parse(args); err != nil {
		return err
	}

	paths, cfg, err := loadPathsAndConfig(*cfgDir, *stateDir, *listen)
	if err != nil {
		return err
	}

	sh := strings.ToLower(strings.TrimSpace(*shell))
	if sh == "" {
		sh = detectShell()
	}

	var pairs [][2]string
	if *unset {
		// Unsetting needs only the variable names: no key and no daemon.
		pairs = EnvPairs("")
	} else {
		var key localauth.Key
		if *check {
			// Print nothing unless the listener proves it holds this config's key.
			k, ok := loadLocalKey(paths)
			if !ok || !service.ProbeHealth(context.Background(), cfg.ListenAddr, k) {
				return nil
			}
			key = k
		} else {
			k, err := localauth.LoadOrCreate(paths.ConfigDir)
			if err != nil {
				return err
			}
			key = k
		}
		pairs = EnvPairs("http://" + cfg.ListenAddr + key.RoutePrefix())
	}

	for _, kv := range pairs {
		k, v := kv[0], kv[1]
		switch sh {
		case "fish":
			if *unset {
				fmt.Fprintf(a.Stdout, "set -e %s\n", k)
			} else {
				fmt.Fprintf(a.Stdout, "set -gx %s %q\n", k, v)
			}
		case "powershell", "pwsh":
			if *unset {
				fmt.Fprintf(a.Stdout, "Remove-Item Env:%s -ErrorAction SilentlyContinue\n", k)
			} else {
				fmt.Fprintf(a.Stdout, "$env:%s = %q\n", k, v)
			}
		default:
			if *unset {
				fmt.Fprintf(a.Stdout, "unset %s\n", k)
			} else {
				fmt.Fprintf(a.Stdout, "export %s=%q\n", k, v)
			}
		}
	}
	if cfg.OMPViaPlugin || ompOnPath() {
		fmt.Fprint(a.Stdout, ompEnvWrapper(sh, *unset, ompStrippedVars(cfg)))
	}
	return nil
}

// powershellOMPWrapper defines an omp function that hides the proxy variables
// from the omp application and restores them afterwards. %s is the quoted,
// comma-separated variable name list.
const powershellOMPWrapper = `function omp {
    $saved = @{}
    try {
        foreach ($name in %s) {
            if (Test-Path "Env:$name") {
                $saved[$name] = (Get-Item "Env:$name").Value
                Remove-Item "Env:$name"
            }
        }
        & (Get-Command omp -CommandType Application -TotalCount 1 -ErrorAction Stop) @args
    } finally {
        foreach ($name in $saved.Keys) { Set-Item "Env:$name" -Value $saved[$name] }
    }
}
`

// ompStrippedVars lists the variables omp must not see. With omp_via_plugin
// that is every variable `munch env` exports: OMP reads them ahead of its own
// config, so an OMP that already reports through the omp-tokenmunchers plugin
// would otherwise have every call reported a second time by the proxy.
// Otherwise OMP stays proxied except for OLLAMA_HOST: OMP reduces it to
// scheme://host:port, which drops the private route, and any override makes
// its Ollama discovery mandatory, so it would fail against the proxy root.
func ompStrippedVars(cfg config.Config) []string {
	if !cfg.OMPViaPlugin {
		return []string{"OLLAMA_HOST"}
	}
	pairs := EnvPairs("")
	names := make([]string, len(pairs))
	for i, kv := range pairs {
		names[i] = kv[0]
	}
	return names
}

// ompOnPath reports whether Oh My Pi can be found, which is when `munch env`
// defines the omp wrapper even without omp_via_plugin.
func ompOnPath() bool {
	_, ok := omp.Find()
	return ok
}

// isOMPCommand reports whether command starts Oh My Pi, judging by its base name.
func isOMPCommand(command string) bool {
	base := filepath.Base(command)
	if runtime.GOOS == "windows" {
		base = strings.TrimSuffix(strings.ToLower(base), ".exe")
	}
	return base == "omp"
}

// withoutEnv returns env without the variables called names.
func withoutEnv(env, names []string) []string {
	kept := env[:0:0]
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, n := range names {
			if name == n || (runtime.GOOS == "windows" && strings.EqualFold(name, n)) {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, kv)
		}
	}
	return kept
}

// ompEnvWrapper returns shell code that defines (or, with unset, removes) an
// omp function running Oh My Pi without the variables in names (see
// ompStrippedVars). The sh and fish bodies start omp through `command env`,
// which looks it up on PATH instead of calling the function again.
func ompEnvWrapper(sh string, unset bool, names []string) string {
	switch sh {
	case "fish":
		if unset {
			return "functions -e omp\n"
		}
		return "function omp; command env -u " + strings.Join(names, " -u ") + " omp $argv; end\n"
	case "powershell", "pwsh":
		if unset {
			return "Remove-Item Function:omp -ErrorAction SilentlyContinue\n"
		}
		return fmt.Sprintf(powershellOMPWrapper, "'"+strings.Join(names, "', '")+"'")
	default:
		if unset {
			return "unset -f omp\n"
		}
		return "omp() { command env -u " + strings.Join(names, " -u ") + " omp \"$@\"; }\n"
	}
}

// envHookLine returns the profile line that routes new shells through munch
// for shell (as reported by detectShell), repeating the install flags that tell
// `munch env` where the proxy is.
func envHookLine(shell string, flags []envFlag) string {
	var args strings.Builder
	for _, f := range flags {
		args.WriteString(" " + f.name + " " + quoteArg(shell, f.value))
	}
	switch shell {
	case "fish":
		return "munch env --check --shell fish" + args.String() + " | source"
	case "powershell":
		return "munch env --shell powershell" + args.String() + " | Out-String | Invoke-Expression"
	default:
		return `eval "$(munch env --check` + args.String() + `)"`
	}
}

// quoteArg quotes s as one word of shell syntax: unchanged when plainly safe,
// single-quoted otherwise.
func quoteArg(shell, s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool { return !isPlainArgRune(r) }) < 0 {
		return s
	}
	switch shell {
	case "powershell":
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	case "fish":
		return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
	default:
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
}

func isPlainArgRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("_./:-", r)
}

func (a *App) cmdExec(args []string) int {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	cfgDir, stateDir := addPathFlags(fs)
	listen := fs.String("listen", "", "override listen address")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cmdArgs := fs.Args()
	if len(cmdArgs) == 0 {
		fmt.Fprintln(a.Stderr, "usage: munch exec [--listen addr] -- <command> [args...]")
		return 2
	}
	paths, cfg, err := loadPathsAndConfig(*cfgDir, *stateDir, *listen)
	if err != nil {
		fmt.Fprintf(a.Stderr, "error: %v\n", err)
		return 1
	}

	// Never hand the child a capability URL (or run it) unless a proxy that
	// holds our local key is ready.
	key, ok := loadLocalKey(paths)
	if !ok || !service.ProbeHealth(context.Background(), cfg.ListenAddr, key) {
		fmt.Fprintln(a.Stderr, "error: munch is not running; start it with `munch start` (or `munch install`)")
		return 1
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Env = cmd.Environ()
	for _, kv := range EnvPairs("http://" + cfg.ListenAddr + key.RoutePrefix()) {
		cmd.Env = append(cmd.Env, kv[0]+"="+kv[1])
	}
	if isOMPCommand(cmdArgs[0]) {
		// Drop the variables omp must not see, including any the user's
		// shell already exported.
		cmd.Env = withoutEnv(cmd.Env, ompStrippedVars(cfg))
		if cfg.OMPViaPlugin {
			fmt.Fprintln(a.Stderr, "munch: OMP reports through omp-tm, so it runs without the proxy variables")
		}
	}
	cmd.Stdin = a.Stdin
	cmd.Stdout = a.Stdout
	cmd.Stderr = a.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(a.Stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func (a *App) cmdStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	cfgDir, stateDir := addPathFlags(fs)
	period := fs.String("period", "today", "time window: today, week, or all (UTC)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	paths, err := resolvePaths(*cfgDir, *stateDir)
	if err != nil {
		return err
	}
	store := localstore.New(paths.UsageLogFile())
	sum, err := store.Summarize(*period, time.Now())
	if err != nil {
		return err
	}

	fmt.Fprintf(a.Stdout, "Local proxy usage (%s, UTC)\n", sum.Period)
	fmt.Fprintf(a.Stdout, "  Calls:       %d\n", sum.Calls)
	fmt.Fprintf(a.Stdout, "  Total:       %s tokens (in=%s out=%s cache_r=%s cache_w=%s)\n",
		formatTokens(sum.TotalTokens),
		formatTokens(sum.InputTokens),
		formatTokens(sum.OutputTokens),
		formatTokens(sum.CacheReadTokens),
		formatTokens(sum.CacheWriteTokens))
	fmt.Fprintf(a.Stdout, "  Est/Rep USD: $%.4f\n", sum.CostUSD)

	if len(sum.ByModel) > 0 {
		fmt.Fprintln(a.Stdout)
		tw := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "PROVIDER\tMODEL\tCALLS\tTOTAL\tCOST")
		for _, m := range sum.ByModel {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t$%.4f\n",
				m.Provider, m.Model, m.Calls, formatTokens(m.TotalTokens), m.CostUSD)
		}
		_ = tw.Flush()
	}
	return nil
}

func (a *App) cmdRoutes(args []string) error {
	fs := flag.NewFlagSet("routes", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	cfgDir, stateDir := addPathFlags(fs)
	listen := fs.String("listen", "", "override listen address")
	showLocal := fs.Bool("show-local-urls", false, "print real local URLs including the private capability path (treat as a secret)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths, cfg, err := loadPathsAndConfig(*cfgDir, *stateDir, *listen)
	if err != nil {
		return err
	}

	// The route path prefix is a capability secret: redact unless explicitly requested.
	prefix := "/_tm/<redacted>"
	if *showLocal {
		key, err := localauth.LoadOrCreate(paths.ConfigDir)
		if err != nil {
			return err
		}
		prefix = key.RoutePrefix()
	}
	baseURL := "http://" + cfg.ListenAddr + prefix
	tw := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ROUTE\tLOCAL URL\tUPSTREAM\tPROVIDER\tFORMAT")
	for _, name := range cfg.SortedRouteNames() {
		r := cfg.Routes[name]
		fmt.Fprintf(tw, "%s\t%s/%s\t%s\t%s\t%s\n",
			name, baseURL, name, r.Upstream, r.Provider, r.Format)
	}
	return tw.Flush()
}

func loadPathsAndConfig(cfgDir, stateDir, listen string) (config.Paths, config.Config, error) {
	paths, err := resolvePaths(cfgDir, stateDir)
	if err != nil {
		return config.Paths{}, config.Config{}, err
	}
	cfg, err := config.Load(paths)
	if err != nil {
		return config.Paths{}, config.Config{}, err
	}
	normalized, err := config.NormalizeListenAddr(cfg.ListenAddr)
	if listen != "" {
		normalized, err = config.NormalizeListenAddr(strings.TrimSpace(listen))
	}
	if err != nil {
		return config.Paths{}, config.Config{}, err
	}
	cfg.ListenAddr = normalized
	return paths, cfg, nil
}

// loadLocalKey reads an existing local key without creating one. A missing or
// unreadable key means no daemon of ours can be authenticated, so callers treat
// it as "not running".
func loadLocalKey(paths config.Paths) (localauth.Key, bool) {
	key, err := localauth.Load(paths.ConfigDir)
	if err != nil {
		return localauth.Key{}, false
	}
	return key, true
}

func configuredLabel(key string) string {
	if strings.TrimSpace(key) == "" {
		return "not configured"
	}
	return "configured"
}

func executablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

func detectShell() string {
	if runtime.GOOS == "windows" {
		return "powershell"
	}
	sh := filepath.Base(os.Getenv("SHELL"))
	switch sh {
	case "fish":
		return "fish"
	case "pwsh", "powershell":
		return "powershell"
	default:
		return "sh"
	}
}

func formatTokens(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.2fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}
