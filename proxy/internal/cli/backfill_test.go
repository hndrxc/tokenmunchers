package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/desktop"
)

const backfillTestEpoch = "1970-01-01T00:00:00.000Z"

func TestParseVerifiedWindowCutoff(t *testing.T) {
	epoch := time.Unix(0, 0).UTC()
	midnight := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	now := midnight.Add(123*time.Millisecond + 456*time.Microsecond).In(time.FixedZone("local", -8*60*60))
	for _, tt := range []struct {
		name, before string
		now, want    time.Time
	}{
		{"UTC date", "2026-01-02", now, midnight},
		{"RFC3339 UTC", "2026-01-02T00:00:00.001+00:00", now, midnight.Add(time.Millisecond)},
		{"now floors to a UTC millisecond", "now", now, midnight.Add(123 * time.Millisecond)},
		{"now at a millisecond is still strictly past", "now", midnight, midnight.Add(-time.Millisecond)},
		{"date equal to invocation time", "2026-01-02", midnight, time.Time{}},
		{"future date", "2026-01-03", now, time.Time{}},
		{"future RFC3339", "2026-01-02T00:00:00.124Z", now, time.Time{}},
		{"cutoff equal to epoch", "1970-01-01", now, time.Time{}},
		{"now is case-sensitive", "Now", now, time.Time{}},
		{"now cannot have surrounding spaces", " now ", now, time.Time{}},
		{"invalid calendar date", "2026-02-30", now, time.Time{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			window, ok := parseVerifiedWindow("", tt.before, tt.now)
			if tt.want.IsZero() {
				if ok {
					t.Fatalf("unexpected verified window: %+v", window)
				}
				return
			}
			if !ok || !window.From.Equal(epoch) || !window.Before.Equal(tt.want) || !window.Before.Before(tt.now) {
				t.Fatalf("parseVerifiedWindow with omitted start and cutoff %q at %v = %+v, %t; want [%v, %v)", tt.before, tt.now, window, ok, epoch, tt.want)
			}
		})
	}
}

func backfillPreviewCutoff(t *testing.T, out string) string {
	t.Helper()
	const prefix = "  Window:     " + backfillTestEpoch + " (inclusive) to "
	for _, line := range strings.Split(out, "\n") {
		if cutoff, ok := strings.CutPrefix(line, prefix); ok {
			if cutoff, ok := strings.CutSuffix(cutoff, " (exclusive)"); ok {
				return cutoff
			}
		}
	}
	t.Fatalf("missing verified epoch-start window in %q", out)
	return ""
}

func TestBackfillRequiresVerifiedWindowAndConsent(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	if err := os.Mkdir(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	const fixture = `{"timestamp":"2026-01-01T00:00:00.000Z","type":"session_meta","payload":{"id":"11111111-1111-4111-8111-111111111111","source":"cli","model_provider":"openai","cwd":"/private-dir"}}
{"timestamp":"2026-01-01T01:00:00.000Z","type":"turn_context","payload":{"turn_id":"turn-1","model":"gpt-5-codex"}}
{"timestamp":"2026-01-01T01:01:00.456Z","type":"token_usage_record","payload":{"thread_id":"11111111-1111-4111-8111-111111111111","turn_id":"turn-1","response_id":"resp_test","usage":{"input_tokens":100,"cached_input_tokens":30,"cache_write_input_tokens":5,"output_tokens":20,"reasoning_output_tokens":8,"total_tokens":120}}}
{"timestamp":"2026-01-01T01:01:00.457Z","type":"token_usage_record","payload":{"thread_id":"11111111-1111-4111-8111-111111111111","turn_id":"turn-1","response_id":"resp_activation","usage":{"input_tokens":2,"cached_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":0,"total_tokens":3}}}
{"timestamp":"2026-01-01T01:00:30.000Z","type":"token_usage_record","payload":{"thread_id":"11111111-1111-4111-8111-111111111111","turn_id":"turn-1","response_id":"resp_recorded","usage":{"input_tokens":2,"cached_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":0,"total_tokens":3}}}
`
	if err := os.WriteFile(filepath.Join(sessions, "rollout-2026-01-01T00-00-00-test.jsonl"), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	key := "tm_" + strings.Repeat("a", 43)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/functions/v1/contributions" || r.Header.Get("Authorization") != "Bearer "+key {
			t.Error("upload must use the authenticated contributions endpoint")
		}
		var req struct {
			Action       string `json:"action"`
			SnapshotID   string `json:"snapshot_id"`
			BackfillFrom string `json:"backfill_from"`
			Cutoff       string `json:"cutoff"`
			Observations []struct {
				CanonicalID string `json:"canonical_id"`
			} `json:"observations"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		ack := map[string]any{"ok": true, "action": req.Action, "snapshot_id": req.SnapshotID}
		if req.Action == "begin" && (req.BackfillFrom != backfillTestEpoch || req.Cutoff != "2026-01-02T00:00:00.000Z") {
			t.Errorf("staged window: [%q, %q); want epoch through exclusive UTC midnight", req.BackfillFrom, req.Cutoff)
		}
		if req.Action == "chunk" && (len(req.Observations) != 1 || req.Observations[0].CanonicalID != "resp_test") {
			t.Error("confirmed chunk did not contain the observed response")
		}
		if req.Action == "commit" {
			ack["committed"], ack["accepted"] = true, 1
		}
		if err := json.NewEncoder(w).Encode(ack); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	t.Setenv("TOKENMUNCHERS_URL", srv.URL+"/functions/v1/ingest")
	t.Setenv("TOKENMUNCHERS_KEY", key)
	cfgDir, stateDir := t.TempDir(), t.TempDir()
	configPath := filepath.Join(cfgDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"desktop_codex_since":"2026-01-01T01:01:00.457Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(stdin string, extra ...string) (int, string, string) {
		args := []string{"backfill", "--codex-home", root, "--config-dir", cfgDir, "--state-dir", stateDir}
		return runApp(stdin, append(args, extra...)...)
	}
	window := []string{"--from", "2026-01-01T00:00:00Z", "--before", "2026-01-02T00:00:00Z"}
	code, out, stderr := run("", window...)
	if code != 0 || !strings.Contains(out, "Eligible:   2 records") || !strings.Contains(out, "desktop_collected: 1") {
		t.Fatalf("local preview: exit %d, stdout %q, stderr %q", code, out, stderr)
	}
	dateWindow := []string{"--before", "2026-01-02"}
	if code, out, stderr := run("", dateWindow...); code != 0 || !strings.Contains(out, "Eligible:   2 records") ||
		backfillPreviewCutoff(t, out) != "2026-01-02T00:00:00.000Z" {
		t.Fatalf("date-only preview: exit %d, stdout %q, stderr %q", code, out, stderr)
	}
	if code, out, stderr := run(""); code != 0 ||
		!strings.Contains(out, "Window: all retained history") || !strings.Contains(out, "Candidates: 2 records") ||
		!strings.Contains(out, "Upload requires an explicit pre-proxy --before") ||
		!strings.Contains(out, "desktop_collected: 1") || !strings.Contains(out, "total=123") ||
		strings.Contains(out, "Eligible:") {
		t.Fatalf("preview without a window: exit %d, stdout %q, stderr %q", code, out, stderr)
	}
	future := time.Now().UTC().AddDate(1, 0, 0).Format(time.RFC3339)
	if code, out, stderr := run("yes\n", "--upload"); code == 0 || out != "" ||
		!strings.Contains(stderr, "upload requires a valid, independently verified") || !strings.Contains(stderr, "--before") {
		t.Fatalf("upload without a window: exit %d, stdout %q, stderr %q", code, out, stderr)
	}
	for _, bounds := range [][]string{
		{"--from", window[1]},
		{"--from=", "--before", window[3]},
		{"--from="},
		{"--before="},
		{"--from=", "--before="},
		{"--from", "not-a-date", "--before", window[3]},
		{"--from", "2026-01-01", "--before", window[3]},
		{"--from", window[1], "--before", future},
		{"--from", "2026-01-01T00:00:00.1234Z", "--before", window[3]},
		{"--from", "2026-01-01T00:00:00.1230Z", "--before", window[3]},
		{"--from", "2026-01-01T00:00:00,123Z", "--before", window[3]},
		{"--from", window[3], "--before", window[1]},
	} {
		if code, out, stderr := run("", bounds...); code == 0 || out != "" ||
			!strings.Contains(stderr, "--before") {
			t.Errorf("preview with window %v: exit %d, stdout %q, stderr %q", bounds, code, out, stderr)
		}
		if code, out, stderr := run("yes\n", append([]string{"--upload"}, bounds...)...); code == 0 || out != "" ||
			!strings.Contains(stderr, "upload requires a valid, independently verified") {
			t.Errorf("upload with window %v: exit %d, stdout %q, stderr %q", bounds, code, out, stderr)
		}
	}
	for _, stdin := range []string{"", "\n", "no\n"} {
		if code, out, stderr := run(stdin, append([]string{"--upload"}, dateWindow...)...); code != 0 || !strings.Contains(out, "Upload cancelled.") {
			t.Fatalf("declined upload: exit %d, stdout %q, stderr %q", code, out, stderr)
		}
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil || len(entries) != 0 || requests.Load() != 0 {
		t.Fatalf("unconfirmed runs wrote state or hit network: %v, %v, %d requests", entries, err, requests.Load())
	}
	desktopState := filepath.Join(stateDir, "desktop")
	oldActivation := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	collector, err := desktop.Open(desktop.Options{
		Dir: desktopState, IngestURL: srv.URL + "/functions/v1/ingest", APIKey: key, CodexSince: oldActivation,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer collector.Close()
	if err := collector.Record(desktop.Observation{
		Harness: desktop.Codex, CanonicalID: "resp_recorded", IdentityStrength: "response",
		Provider: "openai", Model: "gpt-5-codex", Timestamp: oldActivation.Add(30 * time.Second), InputTokens: 2, OutputTokens: 1,
	}); err != nil {
		t.Fatal(err)
	}
	code, out, stderr = run("", window...)
	if code != 0 || !strings.Contains(out, "Eligible:   1 records") || !strings.Contains(out, "desktop_collected: 2") ||
		!strings.Contains(out, "total=120") || requests.Load() != 0 {
		t.Fatalf("desktop-aware preview: exit %d, stdout %q, stderr %q, %d requests", code, out, stderr, requests.Load())
	}
	code, out, stderr = run("yes\n", append([]string{"--upload"}, dateWindow...)...)
	if code != 0 || requests.Load() != 3 || !strings.Contains(out, "1 of 1 records accepted") {
		t.Fatalf("confirmed upload: exit %d, %d requests, stdout %q, stderr %q", code, requests.Load(), out, stderr)
	}
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	code, out, stderr = run("", window...)
	if code != 0 || !strings.Contains(out, "Eligible:   2 records") || !strings.Contains(out, "desktop_collected: 1") {
		t.Fatalf("disabled collector preview: exit %d, stdout %q, stderr %q", code, out, stderr)
	}

	if err := collector.Close(); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(desktopState, "ledger.jsonl")
	before, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("torn tail", func(t *testing.T) {
		// The collector drops an incomplete last line on recovery; it may be an append in progress.
		torn := string(before) + `{"kind":"record"`
		if err := os.WriteFile(ledger, []byte(torn), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out, stderr := run("", window...)
		if code != 0 || !strings.Contains(out, "Eligible:   2 records") || !strings.Contains(out, "desktop_collected: 1") {
			t.Fatalf("torn tail preview: exit %d, stdout %q, stderr %q", code, out, stderr)
		}
		if got, err := os.ReadFile(ledger); err != nil || string(got) != torn {
			t.Fatalf("backfill modified the ledger: %v", err)
		}
	})
	t.Run("malformed line", func(t *testing.T) {
		corrupt := string(before) + "{invalid}\n"
		if err := os.WriteFile(ledger, []byte(corrupt), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, extra := range [][]string{window, append([]string{"--upload"}, window...)} {
			code, out, stderr := run("yes\n", extra...)
			if code == 0 || !strings.Contains(stderr, "read desktop ownership for backfill") || requests.Load() != 3 {
				t.Fatalf("corrupt ledger: exit %d, stdout %q, stderr %q, %d requests", code, out, stderr, requests.Load())
			}
		}
		if got, err := os.ReadFile(ledger); err != nil || string(got) != corrupt {
			t.Fatalf("backfill modified the corrupt ledger: %v", err)
		}
	})
}

func TestClaudeBackfillPreviewAndConsent(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "projects", "test-project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	const session = "7c0ffee0-0000-4000-8000-000000000002"
	const fixture = `{"type":"assistant","timestamp":"2026-01-01T00:00:00.000Z","uuid":"4dd62612-d2a0-4816-bfb7-e4afa81f8aa3","sessionId":"` + session + `","entrypoint":"cli","isSidechain":false,"requestId":"req_test","message":{"id":"msg_test","model":"claude-haiku-5-5","stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":83,"cache_read_input_tokens":4,"cache_creation_input_tokens":1700},"content":[{"type":"text","text":"private transcript text"}]}}
{"type":"cost-state","sessionId":"` + session + `","startTime":1767225600000,"totalCostUSD":0.0004415,"modelUsage":{"claude-haiku-5-5":{"inputTokens":2,"outputTokens":83,"cacheReadInputTokens":4,"cacheCreationInputTokens":1700}}}
`
	if err := os.WriteFile(filepath.Join(project, session+".jsonl"), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	t.Setenv("TOKENMUNCHERS_URL", srv.URL+"/functions/v1/ingest")
	t.Setenv("TOKENMUNCHERS_KEY", "tm_"+strings.Repeat("a", 43))
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	cfgDir, stateDir := t.TempDir(), t.TempDir()
	run := func(stdin string, extra ...string) (int, string, string) {
		args := []string{"backfill", "--harness", "claude-code", "--config-dir", cfgDir, "--state-dir", stateDir}
		return runApp(stdin, append(args, extra...)...)
	}
	code, out, stderr := run("")
	if code != 0 || !strings.Contains(out, "Window: all retained history") || !strings.Contains(out, "Candidates: 1 records") ||
		!strings.Contains(out, "Upload requires an explicit pre-proxy --before") ||
		!strings.Contains(out, "input=2 output=83 cache_read=4 cache_write=1700 total=1789") ||
		strings.Contains(out, "Eligible:") || strings.Contains(out, "private transcript text") {
		t.Fatalf("local preview: exit %d, stdout %q, stderr %q", code, out, stderr)
	}
	if code, out, stderr := run("yes\n", "--upload", "--from", "2026-01-01T00:00:00Z"); code == 0 ||
		!strings.Contains(stderr, "upload requires a valid, independently verified") {
		t.Fatalf("incomplete window: exit %d, stdout %q, stderr %q", code, out, stderr)
	}
	for _, before := range []string{"2026-01-01", "2026-01-01T00:00:00Z"} {
		if code, out, stderr := run("", "--before", before); code != 0 || !strings.Contains(out, "Eligible:   0 records") ||
			backfillPreviewCutoff(t, out) != "2026-01-01T00:00:00.000Z" {
			t.Fatalf("exclusive UTC midnight cutoff %q: exit %d, stdout %q, stderr %q", before, code, out, stderr)
		}
	}
	started := time.Now()
	code, out, stderr = run("\n", "--upload", "--before", "now")
	if code != 0 || !strings.Contains(out, "Eligible:   1 records") || !strings.Contains(out, "Upload cancelled.") {
		t.Fatalf("default-no now upload: exit %d, stdout %q, stderr %q", code, out, stderr)
	}
	resolved := backfillPreviewCutoff(t, out)
	cutoff, err := time.Parse("2006-01-02T15:04:05.000Z", resolved)
	if err != nil || cutoff.Before(started.Add(-time.Millisecond)) || !cutoff.Before(time.Now()) {
		t.Fatalf("resolved now cutoff %q is not an invocation-time UTC millisecond: %v", resolved, err)
	}
	if code, fixed, stderr := run("", "--before", resolved); code != 0 || !strings.Contains(fixed, "Eligible:   1 records") ||
		backfillPreviewCutoff(t, fixed) != resolved {
		t.Fatalf("resolved now cutoff replay: exit %d, stdout %q, stderr %q", code, fixed, stderr)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "missing"))
	if code, out, stderr := run(""); code != 0 || !strings.Contains(out, "Candidates: 0 records") {
		t.Fatalf("missing default home: exit %d, stdout %q, stderr %q", code, out, stderr)
	}
	code, out, stderr = run("\n", "--claude-home", root, "--upload",
		"--from", "2026-01-01T00:00:00Z", "--before", "2026-01-02T00:00:00Z")
	if code != 0 || !strings.Contains(out, "Eligible:   1 records") || !strings.Contains(out, "Upload cancelled.") {
		t.Fatalf("default-no upload: exit %d, stdout %q, stderr %q", code, out, stderr)
	}
	if entries, err := os.ReadDir(stateDir); err != nil || len(entries) != 0 || requests.Load() != 0 {
		t.Fatalf("unconfirmed runs wrote state or hit network: %v, %v, %d requests", entries, err, requests.Load())
	}
}

func TestBackfillRejectsIncompatibleHomeFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--claude-home", "unused"},
		{"--harness", "codex", "--claude-home="},
		{"--harness", "claude-code", "--codex-home", "unused"},
		{"--harness", "claude-code", "--codex-home=", "--claude-home", "unused"},
	} {
		if code, _, stderr := runApp("", append([]string{"backfill"}, args...)...); code == 0 || !strings.Contains(stderr, "incompatible") {
			t.Errorf("backfill %v: exit %d, stderr %q", args, code, stderr)
		}
	}
}
