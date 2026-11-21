package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/config"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/selfupdate"
)

const updateUsage = `Usage: munch update

Replace this munch binary with the latest stable release from GitHub. The
download is checked against the release's SHA256SUMS before it is used, and the
installed binary is left untouched if anything fails. A running background
proxy keeps the old version until "munch restart". Configuration, credentials,
and the service are never touched.
`

// selfUpdateOptions describes the running build to selfupdate. It is a
// variable so tests can serve releases from a fixture and point the update at
// a scratch file; no flag, environment variable, or config file changes where
// updates come from or which file is replaced.
var selfUpdateOptions = func() (selfupdate.Options, error) {
	exe, err := os.Executable()
	if err != nil {
		return selfupdate.Options{}, fmt.Errorf("resolve executable path: %w", err)
	}
	return selfupdate.Options{
		Version:    config.Version,
		GOOS:       runtime.GOOS,
		GOARCH:     runtime.GOARCH,
		Executable: exe,
	}, nil
}

// cmdUpdate replaces this binary with the newest stable release. It never
// restarts the background proxy or touches the service, credentials, or
// config: a running proxy picks up the new binary on `munch restart`.
func (a *App) cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	fs.Usage = func() { fmt.Fprint(fs.Output(), updateUsage) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // the flag package already printed the usage
		}
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("update takes no arguments")
	}

	opts, err := selfUpdateOptions()
	if err != nil {
		return err
	}
	opts.Logf = func(format string, v ...any) {
		fmt.Fprintf(a.Stdout, format+"\n", v...)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	res, err := selfupdate.Update(ctx, opts)
	if err != nil {
		return err
	}
	switch {
	case res.Updated:
		fmt.Fprintf(a.Stdout, "Updated munch %s -> %s (%s)\n", res.Current, res.Latest, res.Path)
		fmt.Fprintf(a.Stdout, "If the background proxy is already running, run `munch restart` to switch it to %s.\n", res.Latest)
	case res.Current == res.Latest:
		fmt.Fprintf(a.Stdout, "munch %s is already the latest release\n", res.Current)
	default:
		fmt.Fprintf(a.Stdout, "munch %s is newer than the latest published release (%s); nothing to update\n", res.Current, res.Latest)
	}
	return nil
}
