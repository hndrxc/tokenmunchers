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
	if code, out, stderr := run(""); code != 0 || !strings.Contains(out, "0 records") {
		t.Fatalf("preview without a window: exit %d, stdout %q, stderr %q", code, out, stderr)
	}
	future := time.Now().UTC().AddDate(1, 0, 0).Format(time.RFC3339)
	for _, bounds := range [][]string{
		nil,
		{"--from", window[1], "--before", future},
		{"--from", "2026-01-01T00:00:00.1234Z", "--before", window[3]},
		{"--from", window[3], "--before", window[1]},
	} {
		if code, out, _ := run("yes\n", append([]string{"--upload"}, bounds...)...); code == 0 {
			t.Fatalf("upload accepted invalid window %v: %s", bounds, out)
		}
	}
	for _, stdin := range []string{"", "\n", "no\n"} {
		if code, out, stderr := run(stdin, append([]string{"--upload"}, window...)...); code != 0 || !strings.Contains(out, "Upload cancelled.") {
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
	code, out, stderr = run("yes\n", append([]string{"--upload"}, window...)...)
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
