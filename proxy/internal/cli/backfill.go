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
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/codex"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/config"
)

const backfillUsage = `Usage: munch backfill [flags]

Preview retained Codex rollout history for backfill reporting. By default this
prints a local preview only: nothing is read except the Codex root, and nothing
is written or uploaded. --from and --before must both be set to an explicitly
verified interval in RFC3339 UTC (at most millisecond precision, not future,
from < before) covering this source, all included archives, and both ChatGPT
and API-key routes; without them no upload is allowed. --upload uploads after
an interactive confirmation that defaults to no; there is no --yes bypass.
`

func (a *App) cmdBackfill(args []string) error {
	fs := flag.NewFlagSet("backfill", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), backfillUsage)
		fs.PrintDefaults()
	}
	cfgDir, stateDir := addPathFlags(fs)
	codexHome := fs.String("codex-home", "", "override Codex home directory (default: $CODEX_HOME or ~/.codex)")
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

	root, err := resolveBackfillRoot(*codexHome)
	if err != nil {
		return err
	}
	window, verified := parseVerifiedWindow(*fromStr, *beforeStr)

	preview, err := backfill.Scan(context.Background(), root, window)
	if err != nil {
		return err
	}
	a.printBackfillPreview(root, window, verified, preview)

	if !*upload {
		return nil
	}
	if !verified {
		return errors.New("upload requires a valid, independently verified --from/--before window")
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
	ok, err := p.askConfirmation(fmt.Sprintf("Confirm this entire window was pre-proxy for this source and upload %d records?", eligible), false)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(a.Stdout, "Upload cancelled.")
		return nil
	}

	paths, err := resolvePaths(*cfgDir, *stateDir)
	if err != nil {
		return err
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

	accepted, err := backfill.Upload(context.Background(), cfg.IngestURL, cfg.APIKey, paths.StateDir, root, window, preview.Records)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Committed snapshot receipt: %d of %d records accepted, %d withheld by the ledger.\n",
		accepted, eligible, eligible-accepted)
	return nil
}

// resolveBackfillRoot resolves the Codex root to scan. --codex-home overrides
// the default home ($CODEX_HOME or <home>/.codex). The result is absolute and,
// when it exists, symlink-resolved. A missing default home behaves as empty:
// nothing is scanned.
func resolveBackfillRoot(override string) (string, error) {
	dir := override
	defaultHome := dir == ""
	if defaultHome {
		home, err := os.UserHomeDir()
		if err != nil && os.Getenv("CODEX_HOME") == "" {
			return "", errors.New("cannot locate Codex home; supply --codex-home")
		}
		dir = codex.Home(os.Getenv, home)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve Codex home: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if defaultHome && errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve Codex home: %w", err)
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
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	if !strings.HasSuffix(s, "Z") && !strings.HasSuffix(s, "+00:00") {
		return time.Time{}, false
	}
	if _, offset := t.Zone(); offset != 0 {
		return time.Time{}, false
	}
	if t.Nanosecond()%1_000_000 != 0 {
		return time.Time{}, false
	}
	return t, true
}

func (a *App) printBackfillPreview(root string, window backfill.Window, verified bool, preview backfill.Preview) {
	fmt.Fprintln(a.Stdout, "Codex backfill preview")
	if root != "" {
		fmt.Fprintf(a.Stdout, "  Codex home: %q\n", root)
	}
	if verified {
		fmt.Fprintf(a.Stdout, "  Window:     %s (inclusive) to %s (exclusive)\n", utcStamp(window.From), utcStamp(window.Before))
		fmt.Fprintln(a.Stdout, "              Applies to all included archives on ChatGPT and API-key routes.")
	} else {
		fmt.Fprintln(a.Stdout, "  Window:     none; supply independently verified --from and --before (RFC3339 UTC)")
	}
	fmt.Fprintf(a.Stdout, "  Eligible:   %d records\n", len(preview.Records))
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
