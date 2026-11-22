package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const codexTestMeta = `{"type":"session_meta","payload":{"originator":"Codex Desktop","model_provider":"openai","source":"vscode"}}` + "\n"

func codexTestRecord(id, turn, timestamp string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"token_usage_record","payload":{"turn_id":%q,"root_turn_id":"other-turn","response_id":%q,"usage":{"input_tokens":100,"output_tokens":32,"cached_input_tokens":21,"cache_write_input_tokens":9,"reasoning_output_tokens":12,"total_tokens":132}}}`+"\n", timestamp, turn, id)
}

func codexTestFile(t *testing.T, home, rel, content string) string {
	t.Helper()
	path := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func codexTestAppend(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString(content)
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		t.Fatal(err)
	}
}

func TestCodexScannerLiveNativeExactUsage(t *testing.T) {
	home := t.TempDir()
	start := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	ts := "2026-08-01T13:00:01.250+01:00"
	contextLine := `{"type":"turn_context","payload":{"turn_id":"turn-1","model":"gpt-5.5"}}` + "\n"
	otherTurn := `{"type":"turn_context","payload":{"turn_id":"other-turn","model":"gpt-5.4"}}` + "\n"
	path := codexTestFile(t, home, "sessions/2026/08/01/rollout-main.jsonl", codexTestMeta+contextLine+otherTurn+codexTestRecord("resp_existing", "turn-1", ts))
	var got []Observation
	record := func(o Observation) error { got = append(got, o); return nil }
	report := func(err error) { t.Error(err) }
	s := newCodexScanner(home, record, report)
	ctx := context.Background()
	s.scan(ctx)
	if len(got) != 1 || got[0].CanonicalID != "resp_existing" {
		t.Fatalf("first scan did not replay the existing candidate: %+v", got)
	}
	row := codexTestRecord("resp_live", "turn-1", ts)
	snapshot := strings.Replace(codexTestRecord("resp_snapshot", "turn-1", ts), `"type":"token_usage_record"`, `"type":"token_count"`, 1)
	wrapped := strings.Replace(codexTestRecord("resp_wrapped", "turn-1", ts), `"type":"token_usage_record","payload":{`, `"type":"event_msg","payload":{"type":"token_usage_record",`, 1)
	oversized := `{"type":"event_msg","payload":{"unrelated":"` + strings.Repeat("x", codexLineLimit*2) + `"}}` + "\n"
	codexTestAppend(t, path, snapshot+wrapped+oversized+row[:len(row)/2])
	s.scan(ctx)
	if len(got) != 1 {
		t.Fatalf("counted a snapshot, wrapped record, or partial line: %+v", got)
	}
	codexTestAppend(t, path, row[len(row)/2:])
	s.scan(ctx)
	s.scan(ctx)
	want := Observation{Harness: Codex, CanonicalID: "resp_live", IdentityStrength: "response", Provider: "openai", Model: "gpt-5.5", Timestamp: start.Add(1250 * time.Millisecond), InputTokens: 70, OutputTokens: 32, CacheReadTokens: 21, CacheWriteTokens: 9}
	if len(got) != 2 || got[1] != want {
		t.Fatalf("observations = %+v; want existing then %+v", got, want)
	}
	got = nil
	restarted := newCodexScanner(home, record, report)
	restarted.scan(ctx)
	if len(got) != 2 || got[0].CanonicalID != "resp_existing" || got[1] != want {
		t.Fatalf("restart candidates = %+v; want both durable writer candidates", got)
	}
}

func TestCodexScannerRejectsOtherClientsAndInvalidUsage(t *testing.T) {
	home := t.TempDir()
	start := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	var got []Observation
	s := newCodexScanner(home, func(o Observation) error { got = append(got, o); return nil }, func(err error) { t.Error(err) })
	ctx := context.Background()
	s.scan(ctx)
	valid := codexTestRecord("resp_live", "missing-turn", start.Format(time.RFC3339))
	codexTestFile(t, home, "sessions/rollout-cli.jsonl", strings.Replace(codexTestMeta, "Codex Desktop", "codex_cli_rs", 1)+valid)
	codexTestFile(t, home, "sessions/rollout-custom.jsonl", strings.Replace(codexTestMeta, "openai", "custom", 1)+valid)
	codexTestFile(t, home, "sessions/rollout-other-originator.jsonl", strings.Replace(codexTestMeta, "Codex Desktop", "vscode", 1)+valid)
	codexTestFile(t, home, "archived_sessions/rollout-live.jsonl", codexTestMeta+
		strings.Replace(valid, `"cached_input_tokens":21`, `"cached_input_tokens":101`, 1)+
		strings.Replace(valid, `"reasoning_output_tokens":12`, `"reasoning_output_tokens":35`, 1)+
		strings.Replace(valid, `"total_tokens":132`, `"total_tokens":144`, 1)+
		strings.Replace(valid, `"resp_live"`, `"resp_"`, 1)+
		strings.Replace(valid, `"resp_live"`, `"resp_live private"`, 1)+
		strings.Replace(valid, start.Format(time.RFC3339), "invalid timestamp", 1)+valid)
	s.scan(ctx)
	if len(got) != 1 || got[0].Model != "unknown" || got[0].CanonicalID != "resp_live" {
		t.Fatalf("observations = %+v; want only native live response with unknown model", got)
	}
}

func TestCodexScannerRetriesTemporaryFailureWithoutBlockingOtherFiles(t *testing.T) {
	home := t.TempDir()
	start := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	var got []string
	var reports []error
	fail := true
	s := newCodexScanner(home, func(o Observation) error {
		if fail && o.CanonicalID == "resp_first" {
			return errors.New("private detail")
		}
		if o.CanonicalID == "resp_invalid" {
			return fmt.Errorf("private detail: %w", ErrRejectedObservation)
		}
		got = append(got, o.CanonicalID)
		return nil
	}, func(err error) { reports = append(reports, err) })
	s.scan(ctx)
	ts := start.Format(time.RFC3339)
	codexTestFile(t, home, "sessions/rollout-a.jsonl", codexTestMeta+codexTestRecord("resp_first", "turn", ts)+codexTestRecord("resp_invalid", "turn", ts)+codexTestRecord("resp_later", "turn", ts))
	codexTestFile(t, home, "sessions/rollout-b.jsonl", codexTestMeta+codexTestRecord("resp_other", "turn", ts))
	s.scan(ctx)
	if strings.Join(got, ",") != "resp_other" || len(reports) != 1 || reports[0] != errCodexRecord {
		t.Fatalf("rejected scan got %v and reports %v", got, reports)
	}
	fail = false
	s.scan(ctx)
	s.scan(ctx)
	if strings.Join(got, ",") != "resp_other,resp_first,resp_later" || len(reports) != 1 {
		t.Fatalf("retry scan got %v", got)
	}
}
