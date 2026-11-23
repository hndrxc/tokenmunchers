package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/backfill"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/claudesettings"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/codex"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/config"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/desktop"
)

const backfillUsage = `Usage: munch backfill [flags]

Preview retained Codex rollouts or local Claude Code history (CLI and Claude
Desktop Code). With no window flags, preview all retained history locally.
To limit the preview, supply both --from and --before in RFC3339 UTC (at
most millisecond precision, both in the past, from < before). Upload requires
explicit pre-proxy --from and --before bounds. Independently verify that the
entire interval predates Munch proxy use for the selected source, all included
archives, and every included route. Upload requires interactive confirmation
that defaults to no; there is no --yes bypass.
`

func (a *App) cmdBackfill(args []string) error {
	fs := flag.NewFlagSet("backfill", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), backfillUsage)
		fs.PrintDefaults()
	}
	cfgDir, stateDir := addPathFlags(fs)
	harness := fs.String("harness", backfill.HarnessCodex, "history source: codex or claude-code")
	codexHome := fs.String("codex-home", "", "override Codex home directory (default: $CODEX_HOME or ~/.codex)")
	claudeHome := fs.String("claude-home", "", "override Claude home directory (default: $CLAUDE_CONFIG_DIR or ~/.claude)")
	fromStr := fs.String("from", "", "verified interval start, inclusive, RFC3339 UTC")
	beforeStr := fs.String("before", "", "verified interval end, exclusive, RFC3339 UTC")
	upload := fs.Bool("upload", false, "upload after interactive confirmation")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // the flag package already printed the usage
		}
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("backfill takes no arguments")
	}

	var homeOverride, wrongHome string
	switch *harness {
	case backfill.HarnessCodex:
		homeOverride, wrongHome = *codexHome, "claude-home"
	case backfill.HarnessClaude:
		homeOverride, wrongHome = *claudeHome, "codex-home"
	default:
		return fmt.Errorf("unknown --harness %q; choose codex or claude-code", *harness)
	}
	var incompatible, boundsSpecified bool
	fs.Visit(func(f *flag.Flag) {
		incompatible = incompatible || f.Name == wrongHome
		boundsSpecified = boundsSpecified || f.Name == "from" || f.Name == "before"
	})
	if incompatible {
		return fmt.Errorf("--%s is incompatible with --harness %s", wrongHome, *harness)
	}
	var window backfill.Window
	if boundsSpecified || *upload {
		var valid bool
		window, valid = parseVerifiedWindow(*fromStr, *beforeStr)
		if !valid {
			if *upload {
				return errors.New("upload requires a valid, independently verified pre-proxy window with explicit --from and --before bounds")
			}
			return errors.New("supply both --from and --before in RFC3339 UTC (at most millisecond precision, both in the past, from < before)")
		}
	}

	root, err := resolveBackfillRoot(*harness, homeOverride)
	if err != nil {
		return err
	}

	paths, err := resolvePaths(*cfgDir, *stateDir)
	if err != nil {
		return err
	}
	disk, err := config.LoadFile(paths)
	if err != nil {
		return err
	}
	recorded, err := desktop.RecordedIDs(desktopDir(paths), *harness)
	if err != nil {
		return fmt.Errorf("read desktop ownership for backfill: %w", err)
	}
	// Reserve from activation even if the collector's persisted cutoff is
	// later (for example after a clock change). Do not infer gaps from pauses.
	ctx := context.Background()
	ownership := backfill.DesktopOwnership{Since: disk.DesktopCodexSince, RecordedIDs: recorded}
	scan := backfill.Scan
	if *harness == backfill.HarnessClaude {
		ownership.Since = disk.DesktopClaudeSince
		scan = backfill.ScanClaude
	}
	preview, err := scan(ctx, root, window, ownership)
	if err != nil {
		return err
	}
	a.printBackfillPreview(*harness, root, window, preview)

	if !*upload {
		return nil
	}
	if preview.Excluded[backfill.ExclWindowContradiction] > 0 {
		return errors.New("recorded Munch usage contradicts the pre-proxy window")
	}
	eligible := len(preview.Records)
	if eligible == 0 {
		fmt.Fprintln(a.Stdout, "No upload: no eligible records.")
		return nil
	}

	p := newPrompter(a.Stdin, a.Stdout, false)
	ok, err := p.askConfirmation(fmt.Sprintf("Confirm you independently verified this entire window was pre-proxy for this source, all included archives and routes, and upload %d records?", eligible), false)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(a.Stdout, "Upload cancelled.")
		return nil
	}

	cfg, err := config.Load(paths)
	if err != nil {
		return err
	}
	if cfg.APIKey == "" {
		return errors.New("run munch login before uploading")
	}
	if err := config.ValidateAPIKey(cfg.APIKey); err != nil {
		return err
	}

	accepted, err := backfill.Upload(ctx, *harness, cfg.IngestURL, cfg.APIKey, paths.StateDir, root, window, preview.Records)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Committed snapshot receipt: %d of %d records accepted, %d withheld by the ledger.\n",
		accepted, eligible, eligible-accepted)
	return nil
}

// resolveBackfillRoot returns an absolute, symlink-resolved history root.
// A missing default home is treated as an empty source.
func resolveBackfillRoot(harness, dir string) (string, error) {
	label, envName, flagName, resolve := "Codex", "CODEX_HOME", "codex-home", codex.Home
	if harness == backfill.HarnessClaude {
		label, envName, flagName, resolve = "Claude", "CLAUDE_CONFIG_DIR", "claude-home", claudesettings.Dir
	}
	defaultHome := dir == ""
	if defaultHome {
		home, err := os.UserHomeDir()
		envHome := os.Getenv(envName)
		if harness == backfill.HarnessClaude {
			envHome = strings.TrimSpace(envHome)
		}
		if err != nil && envHome == "" {
			return "", fmt.Errorf("cannot locate %s home; supply --%s", label, flagName)
		}
		dir = resolve(os.Getenv, home)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve %s home: %w", label, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if defaultHome && errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve %s home: %w", label, err)
	}
	return resolved, nil
}

// parseVerifiedWindow parses --from and --before into the verified scan
// interval. Both must be RFC3339 with an explicit UTC offset ('Z' or
// '+00:00'), at most millisecond precision, strictly in the past, with
// from < before. On any failure it reports no verified window.
func parseVerifiedWindow(from, before string) (backfill.Window, bool) {
	if strings.TrimSpace(from) == "" || strings.TrimSpace(before) == "" {
		return backfill.Window{}, false
	}
	f, okF := parseUTCRFC3339(from)
	b, okB := parseUTCRFC3339(before)
	if !okF || !okB {
		return backfill.Window{}, false
	}
	now := time.Now()
	if f.Unix() < 0 || !b.Before(now) || !f.Before(b) {
		return backfill.Window{}, false
	}
	return backfill.Window{From: f, Before: b}, true
}

// parseUTCRFC3339 parses s as RFC3339 with an explicit UTC offset and at most
// millisecond precision: whole seconds or fractional seconds up to three
// digits. Any other zone, finer precision, or malformed value is rejected.
func parseUTCRFC3339(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	var local string
	switch {
	case strings.HasSuffix(s, "Z"):
		local = strings.TrimSuffix(s, "Z")
	case strings.HasSuffix(s, "+00:00"):
		local = strings.TrimSuffix(s, "+00:00")
	default:
		return time.Time{}, false
	}
	const secondLen = len("2006-01-02T15:04:05")
	// Go's parser also accepts comma fractions and more than three digits,
	// even when the parsed timestamp lands exactly on a millisecond.
	if len(local) != secondLen &&
		(len(local) < secondLen+2 || len(local) > secondLen+4 || local[secondLen] != '.') {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func (a *App) printBackfillPreview(harness, root string, window backfill.Window, preview backfill.Preview) {
	label, homeLabel := "Codex", "Codex"
	if harness == backfill.HarnessClaude {
		label, homeLabel = "Claude Code (CLI and Desktop local Code)", "Claude"
	}
	fmt.Fprintf(a.Stdout, "%s backfill preview\n", label)
	if root != "" {
		fmt.Fprintf(a.Stdout, "  %s home: %q\n", homeLabel, root)
	}
	if !window.IsZero() {
		fmt.Fprintf(a.Stdout, "  Window:     %s (inclusive) to %s (exclusive)\n", utcStamp(window.From), utcStamp(window.Before))
		if harness == backfill.HarnessClaude {
			fmt.Fprintln(a.Stdout, "              Independently verify this was pre-proxy on every included archive and route.")
		} else {
			fmt.Fprintln(a.Stdout, "              Applies to all included archives on ChatGPT and API-key routes.")
		}
		fmt.Fprintf(a.Stdout, "  Eligible:   %d records\n", len(preview.Records))
	} else {
		fmt.Fprintln(a.Stdout, "  Window: all retained history")
		fmt.Fprintf(a.Stdout, "  Candidates: %d records\n", len(preview.Records))
		fmt.Fprintln(a.Stdout, "  Upload requires explicit pre-proxy --from and --before bounds.")
	}
	var min, max time.Time
	var input, output, cacheRead, cacheWrite int64
	for i, record := range preview.Records {
		if i == 0 || record.Timestamp.Before(min) {
			min = record.Timestamp
		}
		if i == 0 || record.Timestamp.After(max) {
			max = record.Timestamp
		}
		input += record.Input
		output += record.Output
		cacheRead += record.CacheRead
		cacheWrite += record.CacheWrite
	}
	if len(preview.Records) > 0 {
		fmt.Fprintf(a.Stdout, "  Span:       %s .. %s\n", utcStamp(min), utcStamp(max))
	} else {
		fmt.Fprintln(a.Stdout, "  Span:       none")
	}
	fmt.Fprintf(a.Stdout, "  Tokens:     input=%d output=%d cache_read=%d cache_write=%d total=%d\n",
		input, output, cacheRead, cacheWrite, input+output+cacheRead+cacheWrite)
	reasons := make([]string, 0, len(preview.Excluded))
	for r := range preview.Excluded {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	for _, r := range reasons {
		fmt.Fprintf(a.Stdout, "  Excluded:   %s: %d\n", r, preview.Excluded[r])
	}
	fmt.Fprintln(a.Stdout, "  Preview is local. Upload sends only usage metadata, never transcripts or paths.")
	fmt.Fprintln(a.Stdout, "  The server may withhold previously accepted or overlapping usage.")
}

func utcStamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}
