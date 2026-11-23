package desktop

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

var ledgerEpoch = time.Date(2025, 5, 10, 12, 0, 0, 0, time.UTC)

func ledgerOptions(t *testing.T) Options {
	t.Helper()
	return Options{Dir: filepath.Join(t.TempDir(), "desktop"), IngestURL: "https://example.org/functions/v1/ingest", APIKey: "test-key", ClaudeSince: ledgerEpoch}
}

func openLedger(t *testing.T, opts Options) *Collector {
	t.Helper()
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c
}

func ledgerObservation(id string, when time.Time) Observation {
	return Observation{Harness: Claude, CanonicalID: "req_" + id, IdentityStrength: "request", Provider: "anthropic", Timestamp: when,
		InputTokens: 30, OutputTokens: 20, CacheReadTokens: 10, CacheWriteTokens: 5}
}

func ledgerRecord(t *testing.T, c *Collector, id string, at time.Time) {
	t.Helper()
	if err := c.Record(ledgerObservation(id, at)); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopLedgerRestartActivationAndTailRepair(t *testing.T) {
	opts := ledgerOptions(t)
	opts.ClaudeSince = ledgerEpoch.Add(1234 * time.Microsecond)
	c := openLedger(t, opts)
	ledgerRecord(t, c, "old", ledgerEpoch.Add(time.Millisecond))
	ledgerRecord(t, c, "live", opts.ClaudeSince.In(time.FixedZone("offset", 3600)))
	ledgerRecord(t, c, "live", opts.ClaudeSince)
	invalid := ledgerObservation("invalid", opts.ClaudeSince)
	invalid.Model = "something with spaces"
	if err := c.Record(invalid); !errors.Is(err, ErrRejectedObservation) {
		t.Fatalf("invalid metadata returned %v", err)
	}
	device, cutoff := c.sources[Claude].Device, c.sources[Claude].Cutoff
	if c.Pending() != 1 || !cutoff.Equal(ledgerEpoch.Add(time.Millisecond)) {
		t.Fatal("first live record or cutoff incorrect")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(opts.Dir, "ledger.jsonl")
	if runtime.GOOS != "windows" {
		for path, want := range map[string]os.FileMode{opts.Dir: 0700, path: 0600} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != want {
				t.Fatalf("permissions %v, want %v", info.Mode().Perm(), want)
			}
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"kind":"record"`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	opts.ClaudeSince = ledgerEpoch.Add(time.Hour)
	c = openLedger(t, opts)
	if s := c.sources[Claude]; s.Device != device || !s.Cutoff.Equal(cutoff) {
		t.Fatal("source identity or cutoff changed")
	}
	ledgerRecord(t, c, "inactive", opts.ClaudeSince.Add(-time.Nanosecond))
	ledgerRecord(t, c, "live", opts.ClaudeSince)
	ledgerRecord(t, c, "reactivated", opts.ClaudeSince)
	if c.Pending() != 2 {
		t.Fatalf("pending %d, want 2", c.Pending())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = openLedger(t, opts)
	if queue := c.sources[Claude].queue; len(queue) != 2 || queue[1].CanonicalID != "req_reactivated" {
		t.Fatal("record appended after torn-tail recovery did not replay")
	}
}

func TestRecordedIDsSelectsHarnessAndDoesNotRepairLedger(t *testing.T) {
	opts := ledgerOptions(t)
	opts.CodexSince = ledgerEpoch
	ids, err := RecordedIDs(opts.Dir, Claude)
	if err != nil || len(ids) != 0 {
		t.Fatalf("missing ledger: %v, %v", ids, err)
	}
	if _, err := os.Stat(opts.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read created state: %v", err)
	}
	c := openLedger(t, opts)
	ledgerRecord(t, c, "live", ledgerEpoch)
	codex := ledgerObservation("unused", ledgerEpoch)
	codex.Harness, codex.CanonicalID, codex.IdentityStrength, codex.Provider = Codex, "resp_live", "response", "openai"
	if err := c.Record(codex); err != nil {
		t.Fatal(err)
	}
	for harness, want := range map[string]string{Claude: "req_live", Codex: "resp_live"} {
		ids, err := RecordedIDs(opts.Dir, harness)
		if _, ok := ids[want]; err != nil || !ok || len(ids) != 1 {
			t.Fatalf("%s IDs: %v, %v", harness, ids, err)
		}
	}
	if _, err := RecordedIDs(opts.Dir, "unsupported"); err == nil {
		t.Fatal("unsupported harness accepted")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(opts.Dir, "ledger.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A torn final line is ignored while it may be an in-progress append.
	torn := append(before, []byte(`{"kind":"record","harness":"claude-code"`)...)
	if err := os.WriteFile(path, torn, 0600); err != nil {
		t.Fatal(err)
	}
	ids, err = RecordedIDs(opts.Dir, Claude)
	if _, ok := ids["req_live"]; err != nil || !ok || len(ids) != 1 {
		t.Fatalf("torn ledger: %v, %v", ids, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(torn, after) {
		t.Fatalf("read modified ledger: %v", err)
	}
	// A committed but malformed observation fails closed instead.
	invalid := append(before, []byte(`{"kind":"record","harness":"claude-code","observation":{"harness":"claude-code","canonical_id":"msg_wrong"}}`+"\n")...)
	if err := os.WriteFile(path, invalid, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordedIDs(opts.Dir, Claude); !errors.Is(err, errCorrupt) {
		t.Fatalf("invalid record: %v", err)
	}
}

func TestDesktopLedgerPauseWindowsSurviveStoppedMunch(t *testing.T) {
	opts := ledgerOptions(t)
	if err := PersistPause(opts.Dir, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(opts.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("offline pause created state: %v", err)
	}
	c := openLedger(t, opts)
	start, end := ledgerEpoch.Add(time.Hour), ledgerEpoch.Add(2*time.Hour)
	ledgerRecord(t, c, "pre", start.Add(-time.Millisecond))
	if err := c.setPaused(true, start); err != nil {
		t.Fatal(err)
	}
	if err := c.Record(ledgerObservation("seen-while-paused", start.Add(-time.Minute))); !errors.Is(err, errPaused) {
		t.Fatalf("delayed pre-pause record should be retryable, got %v", err)
	}
	ledgerRecord(t, c, "within-pause", start.Add(time.Minute))
	if err := c.setPaused(false, end); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := PersistPause(opts.Dir, true); err != nil {
		t.Fatal(err)
	}
	c = openLedger(t, opts)
	if !c.paused() || c.Pending() != 1 {
		t.Fatal("offline pause did not persist, or prior queue was dropped")
	}
	offlineStart := c.windows[len(c.windows)-1].start
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := PersistPause(opts.Dir, false); err != nil {
		t.Fatal(err)
	}
	c = openLedger(t, opts)
	if c.paused() || len(c.windows) != 2 {
		t.Fatal("offline resume did not retain both windows")
	}
	ledgerRecord(t, c, "pause-start", start)
	ledgerRecord(t, c, "pause-mid", start.Add(time.Minute))
	ledgerRecord(t, c, "pause-last-ms", end)
	ledgerRecord(t, c, "offline-start", offlineStart)
	ledgerRecord(t, c, "seen-while-paused", start.Add(-time.Minute))
	ledgerRecord(t, c, "after-first-pause", end.Add(time.Millisecond))
	if c.Pending() != 3 {
		t.Fatalf("pending %d, want 3", c.Pending())
	}
}

func TestDesktopLedgerInitialPausedConfigExcludesDelayedEvents(t *testing.T) {
	opts := ledgerOptions(t)
	opts.Paused = true
	c := openLedger(t, opts)
	if err := c.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	ledgerRecord(t, c, "delayed-since-opt-in", opts.ClaudeSince.Add(time.Minute))
	if c.Pending() != 0 {
		t.Fatal("initially paused configuration captured delayed usage")
	}
}

func TestDesktopLedgerExclusiveAccess(t *testing.T) {
	opts := ledgerOptions(t)
	c := openLedger(t, opts)
	if other, err := Open(opts); err == nil {
		_ = other.Close()
		t.Fatal("second collector took the same ledger lock")
	}
	if err := PersistPause(opts.Dir, true); err == nil {
		t.Fatal("offline pause proceeded while the collector was running")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := PersistPause(opts.Dir, true); err != nil {
		t.Fatal(err)
	}
	if c := openLedger(t, opts); !c.paused() {
		t.Fatal("offline pause failed after releasing lock")
	}
}

func TestDesktopLedgerWriteFailureDoesNotAcceptRecord(t *testing.T) {
	opts := ledgerOptions(t)
	c := openLedger(t, opts)
	if err := c.journal.file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Record(ledgerObservation("failed", ledgerEpoch)); !errors.Is(err, errLedger) || c.Pending() != 0 {
		t.Fatalf("failed write was not rejected: pending %d, error %v", c.Pending(), err)
	}
	_ = c.Close() // The test already closed the underlying journal to simulate failure.
	c = openLedger(t, opts)
	ledgerRecord(t, c, "failed", ledgerEpoch)
	if c.Pending() != 1 {
		t.Fatal("failed write was treated as an accepted duplicate")
	}
}

func TestDesktopLedgerRotationFailsClosed(t *testing.T) {
	opts := ledgerOptions(t)
	c := openLedger(t, opts)
	ledgerRecord(t, c, "original", ledgerEpoch)
	pending, err := c.prepare(Claude)
	if err != nil || pending == nil {
		t.Fatalf("prepare failed: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	changed := opts
	changed.APIKey += "-rotated"
	if _, err := Open(changed); !errors.Is(err, errBinding) {
		t.Fatalf("key rotation returned %v", err)
	}
	changed = opts
	changed.IngestURL = "https://other.example.org/functions/v1/ingest"
	if _, err := Open(changed); !errors.Is(err, errBinding) {
		t.Fatalf("destination rotation returned %v", err)
	}
	c = openLedger(t, opts)
	if c.Pending() != 1 || c.sources[Claude].pending.ID != pending.ID {
		t.Fatal("rotation altered original pending state")
	}
}
