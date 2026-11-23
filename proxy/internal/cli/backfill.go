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
Supply --before to limit the preview: use YYYY-MM-DD (midnight UTC),
RFC3339 UTC (up to millisecond precision), or now. The cutoff is exclusive
and must be in the past; now resolves once to a strictly past UTC millisecond.
--from is inclusive and must be RFC3339 UTC (up to millisecond precision),
at or after the Unix epoch and earlier than --before. If omitted, it
defaults to 1970-01-01T00:00:00.000Z when --before is supplied.

Upload requires an explicit --before and independent verification that the
entire interval predates Munch proxy use for the selected source, all included
archives, and every included route. Use now only if verified through the
resolved cutoff; use the printed absolute cutoff for any retry. Upload
requires interactive confirmation that defaults to no; there is no --yes bypass.
`

func (a *App) cmdBackfill(args []string) error {
	now := time.Now()
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
	fromStr := fs.String("from", "", "inclusive start: RFC3339 UTC (defaults to Unix epoch with --before)")
	beforeStr := fs.String("before", "", "exclusive cutoff: YYYY-MM-DD (midnight UTC), RFC3339 UTC, or now")
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
	var incompatible, fromSpecified, beforeSpecified bool
	fs.Visit(func(f *flag.Flag) {
		incompatible = incompatible || f.Name == wrongHome
		fromSpecified = fromSpecified || f.Name == "from"
		beforeSpecified = beforeSpecified || f.Name == "before"
	})
	if incompatible {
		return fmt.Errorf("--%s is incompatible with --harness %s", wrongHome, *harness)
	}
	var window backfill.Window
	if fromSpecified || beforeSpecified || *upload {
		var valid bool
		if beforeSpecified && (!fromSpecified || strings.TrimSpace(*fromStr) != "") {
			window, valid = parseVerifiedWindow(*fromStr, *beforeStr, now)
		}
		if !valid {
			const hint = "supply --before as a past YYYY-MM-DD (midnight UTC), RFC3339 UTC (up to millisecond precision), or now; if supplied, --from must be RFC3339 UTC (up to millisecond precision), at or after the Unix epoch and earlier than --before"
			if *upload {
				return errors.New("upload requires a valid, independently verified pre-proxy window: " + hint)
			}
			return errors.New(hint)
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

	if *beforeStr == "now" {
		fmt.Fprintf(a.Stdout, "Use --before now only if you verified the history was pre-proxy through %s.\n", utcStamp(window.Before))
		fmt.Fprintf(a.Stdout, "On retry, reuse --before %s instead of now.\n", utcStamp(window.Before))
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

// parseVerifiedWindow defaults an omitted start to the Unix epoch and
// resolves the exclusive cutoff against the invocation time.
func parseVerifiedWindow(from, before string, now time.Time) (backfill.Window, bool) {
	f := time.Unix(0, 0).UTC()
	if from != "" {
		var ok bool
		f, ok = parseUTCRFC3339(from)
		if !ok {
			return backfill.Window{}, false
		}
	}

	var b time.Time
	switch {
	case before == "now":
		// If now is already on a millisecond boundary, choose the preceding one.
		b = now.Add(-time.Nanosecond).Truncate(time.Millisecond).UTC()
	case len(before) == len(time.DateOnly):
		var err error
		b, err = time.Parse(time.DateOnly, before)
		if err != nil {
			return backfill.Window{}, false
		}
	default:
		var ok bool
		b, ok = parseUTCRFC3339(before)
		if !ok {
			return backfill.Window{}, false
		}
	}
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
		fmt.Fprintln(a.Stdout, "  Upload requires an explicit pre-proxy --before; --from defaults to the Unix epoch.")
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
