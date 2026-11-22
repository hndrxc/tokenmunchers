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
	if err := os.WriteFile(configPath, []byte("{invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(stdin string, extra ...string) (int, string, string) {
		args := []string{"backfill", "--codex-home", root, "--config-dir", cfgDir, "--state-dir", stateDir}
		return runApp(stdin, append(args, extra...)...)
	}
	window := []string{"--from", "2026-01-01T00:00:00Z", "--before", "2026-01-02T00:00:00Z"}
	code, out, stderr := run("", window...)
	if code != 0 || !strings.Contains(out, "total=120") || !strings.Contains(out, "2026-01-01T01:01:00.456Z") {
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
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	code, out, stderr = run("yes\n", append([]string{"--upload"}, window...)...)
	if code != 0 || requests.Load() != 3 || !strings.Contains(out, "1 of 1 records accepted") {
		t.Fatalf("confirmed upload: exit %d, %d requests, stdout %q, stderr %q", code, requests.Load(), out, stderr)
	}
}
