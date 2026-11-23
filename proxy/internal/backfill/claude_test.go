package backfill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const claudeFixtureModel = "claude-fixture"

func claudeFixtureFrame(req, msg, at, entrypoint string, counts claudeCounts) string {
	return fmt.Sprintf(`{"type":"assistant","requestId":%q,"timestamp":%q,"entrypoint":%q,"message":{"id":%q,"model":%q,"content":[{"text":"transcript_secret"}],"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}}`, req, at, entrypoint, msg, claudeFixtureModel, counts[0], counts[1], counts[2], counts[3])
}

func claudeFixtureState(session string, counts claudeCounts) string {
	return fmt.Sprintf(`{"type":"cost-state","sessionId":%q,"modelUsage":{%q:{"inputTokens":%d,"outputTokens":%d,"cacheReadInputTokens":%d,"cacheCreationInputTokens":%d,"costUSD":"free_form_secret"}}}`, session, claudeFixtureModel, counts[0], counts[1], counts[2], counts[3])
}

func claudeFixtureWrite(t *testing.T, root, rel, data string) {
	t.Helper()
	p := filepath.Join(root, "projects", "fixture", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func claudeFixtureLines(t *testing.T, root, rel string, rows ...string) {
	t.Helper()
	claudeFixtureWrite(t, root, rel, strings.Join(rows, "\n")+"\n")
}

func claudeFixtureScan(t *testing.T, root string, w Window, desktop DesktopOwnership) Preview {
	t.Helper()
	// Match CLI root resolution (macOS's default temp path traverses /var).
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ScanClaude(context.Background(), root, w, desktop)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func claudeFixtureIDs(p Preview) []string {
	var ids []string
	for _, r := range p.Records {
		ids = append(ids, r.ID)
	}
	return ids
}

func TestScanClaudeExactSessionEvidenceBeforeFiltering(t *testing.T) {
	root := t.TempDir()
	main := claudeFixtureFrame("req_main", "msg_main", "2026-10-28T02:28:25.705Z", "cli", claudeCounts{2, 22, 0, 608})
	child := claudeFixtureFrame("req_child", "msg_child", "2026-10-28T02:28:33.826Z", "cli", claudeCounts{2, 21, 7, 705})
	outside := claudeFixtureFrame("req_outside", "msg_outside", "2026-10-28T02:28:23Z", "cli", claudeCounts{3, 5, 2, 10})
	claudeFixtureLines(t, root, "parent.jsonl", outside, main, main,
		claudeFixtureState("parent", claudeCounts{5, 27, 2, 618}),
		claudeFixtureState("parent", claudeCounts{7, 48, 9, 1323}))
	claudeFixtureLines(t, root, "parent/subagents/agent-child.jsonl", child, child)
	// An independently verified fork contains the parent frame and new work.
	forkMain := strings.Replace(main, "02:28:25.705", "02:28:35.705", 1)
	forkNew := claudeFixtureFrame("req_fork", "msg_fork", "2026-10-28T02:28:35.826Z", "cli", claudeCounts{1, 3, 4, 5})
	claudeFixtureLines(t, root, "fork.jsonl", forkMain, forkNew, claudeFixtureState("fork", claudeCounts{3, 25, 4, 613}))
	w := Window{From: claudeTime("2026-10-28T02:28:25.705Z"), Before: claudeTime("2026-10-28T02:28:35.826Z")}
	p := claudeFixtureScan(t, root, w, DesktopOwnership{})
	if got := claudeFixtureIDs(p); !reflect.DeepEqual(got, []string{"req_main", "req_child"}) {
		t.Fatalf("IDs = %v; excluded = %v", got, p.Excluded)
	}
	m, c := p.Records[0], p.Records[1]
	if m.ResponseID != "msg_main" || m.Provider != unknownProvider || m.Model != claudeFixtureModel || m.Input != 2 || m.Output != 22 || m.CacheRead != 0 || m.CacheWrite != 608 || m.Subagent == nil || *m.Subagent || m.SessionID != "" || m.ParentSessionID != "" {
		t.Fatalf("incorrect main metadata: %+v", m)
	}
	if c.Subagent == nil || !*c.Subagent || c.Output != 21 || c.CacheRead != 7 || !m.Timestamp.Equal(w.From) {
		t.Fatalf("incorrect child or original attribution: %+v, %+v", c, m)
	}
	if p.Excluded[ExclDuplicate] != 3 || p.Excluded[ExclOutsideWindow] != 2 {
		t.Fatalf("unexpected exclusions: %+v", p.Excluded)
	}
	preview := claudeFixtureScan(t, root, Window{}, DesktopOwnership{})
	if got := claudeFixtureIDs(preview); !reflect.DeepEqual(got, []string{"req_outside", "req_main", "req_child", "req_fork"}) {
		t.Fatalf("preview IDs = %v; excluded = %v", got, preview.Excluded)
	}
	metadata, err := json.Marshal(preview)
	if err != nil || strings.Contains(string(metadata), "secret") {
		t.Fatalf("metadata serialization leaked transcript fields: err=%v", err)
	}
}

func TestScanClaudeRejectsUnverifiedGroups(t *testing.T) {
	at := "2026-10-28T02:28:25.705Z"
	counts := claudeCounts{2, 22, 3, 608}
	frame := claudeFixtureFrame("req_one", "msg_one", at, "cli", counts)
	state := claudeFixtureState("session", counts)
	for _, tc := range []struct {
		name, jsonl, reason string
	}{
		{"missing state", frame + "\n", ExclUnverifiedOutput},
		{"state is for another session", frame + "\n" + claudeFixtureState("other", counts) + "\n", ExclUnverifiedOutput},
		{"last summary unusable", frame + "\n" + state + "\n" + `{"type":"cost-state","sessionId":"session"}` + "\n", ExclUnverifiedOutput},
		{"stale state before message", state + "\n" + frame + "\n", ExclUnverifiedOutput},
		{"inexact summary", frame + "\n" + claudeFixtureState("session", claudeCounts{2, 22, 0, 608}) + "\n", ExclUnverifiedOutput},
		{"unavailable counter", strings.Replace(frame, `"cache_read_input_tokens":3,`, "", 1) + "\n" + state + "\n", ExclUnverifiedOutput},
		{"unavailable summary counter", frame + "\n" + strings.Replace(state, `"cacheReadInputTokens":3,`, "", 1) + "\n", ExclUnverifiedOutput},
		{"changed counter", frame + "\n" + strings.Replace(frame, `"output_tokens":22`, `"output_tokens":1`, 1) + "\n" + state + "\n", ExclConflict},
		{"changed model", frame + "\n" + strings.Replace(frame, claudeFixtureModel, "claude-other", 1) + "\n" + state + "\n", ExclConflict},
		{"truncated tail", frame + "\n" + state + "\n" + `{"type":"assistant"`, ExclIncompleteLine},
		{"corrupt line", frame + "\n{out-of-band error}\n" + state + "\n", ExclMalformedJSON},
		{"rejected usage not dropped", frame + "\n" + claudeFixtureFrame("req_ambiguous", "msg_/unsafe", at, "cli", counts) + "\n" + state + "\n", ExclUnsafeID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			claudeFixtureWrite(t, root, "session.jsonl", tc.jsonl)
			p := claudeFixtureScan(t, root, Window{}, DesktopOwnership{})
			if len(p.Records) != 0 || p.Excluded[tc.reason] == 0 {
				t.Fatalf("expected no records and %s; IDs = %v, excluded = %v", tc.reason, claudeFixtureIDs(p), p.Excluded)
			}
		})
	}
}

func TestScanClaudeGlobalIdentityConflicts(t *testing.T) {
	at := "2026-10-28T02:28:25.705Z"
	counts := claudeCounts{2, 22, 3, 608}
	for _, tc := range []struct {
		name, req, msg string
	}{
		{"request maps to multiple messages", "req_one", "msg_two"},
		{"message maps to multiple requests", "req_two", "msg_one"},
		{"request on unidentified response", "req_one", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			claudeFixtureLines(t, root, "verified.jsonl", claudeFixtureFrame("req_one", "msg_one", at, "cli", counts), claudeFixtureState("verified", counts))
			// A copy lacking a main file or usable summary can still reveal a
			// conflict with a response in a verified session.
			claudeFixtureLines(t, root, "orphan/subagents/agent-copy.jsonl", claudeFixtureFrame(tc.req, tc.msg, at, "cli", counts))
			p := claudeFixtureScan(t, root, Window{}, DesktopOwnership{})
			if len(p.Records) != 0 || p.Excluded[ExclConflict] == 0 {
				t.Fatalf("ambiguous IDs published: %v; excluded = %v", claudeFixtureIDs(p), p.Excluded)
			}
		})
	}
}

func TestScanClaudeDesktopOwnershipBeforeWindow(t *testing.T) {
	root := t.TempDir()
	counts := claudeCounts{2, 22, 3, 608}
	rows := []struct{ id, at, entrypoint string }{
		{"archive", "2026-10-28T02:28:00Z", "claude-desktop"},
		{"forked", "2026-10-28T02:28:01Z", "claude-desktop"},
		{"skew", "2026-10-28T02:28:09.972Z", "claude-desktop"},
		{"desktop", "2026-10-28T02:28:10Z", "claude-desktop"},
		{"unknown", "2026-10-28T02:28:11Z", "unrecognized"},
		{"recorded", "2026-10-28T02:28:12Z", "cli"},
		{"cli", "2026-10-28T02:28:13Z", "cli"},
		{"sdk", "2026-10-28T02:28:14Z", "sdk-cli"},
	}
	var frames []string
	var totals claudeCounts
	for _, row := range rows {
		frames = append(frames, claudeFixtureFrame("req_"+row.id, "msg_"+row.id, row.at, row.entrypoint, counts))
		for i, n := range counts {
			totals[i] += n
		}
	}
	claudeFixtureLines(t, root, "desktop-and-cli.jsonl", append(frames, claudeFixtureState("desktop-and-cli", totals))...)
	claudeFixtureLines(t, root, "orphan/subagents/agent-copy.jsonl", claudeFixtureFrame("req_forked", "msg_forked", "2026-10-28T02:30:00Z", "", counts))
	since := claudeTime("2026-10-28T02:28:10Z")
	w := Window{From: since.Add(-time.Minute), Before: since.Add(time.Minute)}
	p := claudeFixtureScan(t, root, w, DesktopOwnership{Since: since, RecordedIDs: map[string]struct{}{"req_recorded": {}}})
	if got := claudeFixtureIDs(p); !reflect.DeepEqual(got, []string{"req_archive", "req_cli", "req_sdk"}) || p.Excluded[ExclDesktopCollected] != 5 {
		t.Fatalf("ownership: IDs = %v, excluded = %v", got, p.Excluded)
	}
}

func TestScanClaudeDoesNotReadLinkedSubagents(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	counts := claudeCounts{2, 22, 3, 608}
	at := "2026-10-28T02:28:25.705Z"
	claudeFixtureLines(t, root, "session.jsonl", claudeFixtureFrame("req_main", "msg_main", at, "cli", counts), claudeFixtureState("session", counts))
	claudeFixtureLines(t, outside, "agent-other.jsonl", claudeFixtureFrame("req_main", "msg_conflict", at, "cli", counts))
	session := filepath.Join(root, "projects", "fixture", "session")
	if err := os.Mkdir(session, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "projects", "fixture"), filepath.Join(session, "subagents")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	p := claudeFixtureScan(t, root, Window{}, DesktopOwnership{})
	if len(p.Records) != 0 || p.Excluded[ExclUnverifiedOutput] != 1 || p.Excluded[ExclConflict] != 0 {
		t.Fatalf("linked subagent affected evidence: IDs = %v, exclusions = %v", claudeFixtureIDs(p), p.Excluded)
	}
}

func TestScanClaudeIncompleteCopyCannotSupplyRequestIdentity(t *testing.T) {
	root := t.TempDir()
	counts := claudeCounts{2, 22, 3, 608}
	at := "2026-10-28T02:28:25.705Z"
	claudeFixtureLines(t, root, "verified.jsonl", claudeFixtureFrame("", "msg_one", at, "cli", counts), claudeFixtureState("verified", counts))
	claudeFixtureWrite(t, root, "truncated.jsonl", claudeFixtureFrame("req_one", "msg_one", at, "cli", counts)+"\n{")
	p := claudeFixtureScan(t, root, Window{}, DesktopOwnership{})
	if len(p.Records) != 0 || p.Excluded[ExclMissingIdentity] != 1 {
		t.Fatalf("incomplete copy supplied a publishable pairing: %v; excluded = %v", claudeFixtureIDs(p), p.Excluded)
	}
}

func TestScanClaudeForkCannotOmitInheritedUsageFromItsEvidence(t *testing.T) {
	root := t.TempDir()
	counts := claudeCounts{2, 22, 3, 608}
	inherited := claudeFixtureFrame("req_one", "msg_one", "2026-10-28T02:28:25.705Z", "cli", counts)
	newFrame := claudeFixtureFrame("req_two", "msg_two", "2026-10-28T02:28:27Z", "cli", counts)
	claudeFixtureLines(t, root, "parent.jsonl", inherited, claudeFixtureState("parent", counts))
	claudeFixtureLines(t, root, "fork.jsonl", inherited, newFrame, claudeFixtureState("fork", counts))
	p := claudeFixtureScan(t, root, Window{}, DesktopOwnership{})
	if got := claudeFixtureIDs(p); !reflect.DeepEqual(got, []string{"req_one"}) || p.Excluded[ExclUnverifiedOutput] != 1 {
		t.Fatalf("fork verification: IDs = %v, excluded = %v", got, p.Excluded)
	}
}
