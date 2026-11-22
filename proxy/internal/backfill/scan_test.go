package backfill

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

var (
	winFrom    = time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	winBefore  = time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	testWindow = Window{From: winFrom, Before: winBefore}
)

func strp(s string) *string { return &s }
func u64p(v uint64) *uint64 { return &v }
func i64p(v int64) *int64   { return &v }
func ts(s string) *string   { return &s }

func envLine(tstamp *string, ord *uint64, typ string, payload any) []byte {
	m := map[string]any{"type": typ, "payload": payload}
	if tstamp != nil {
		m["timestamp"] = *tstamp
	}
	if ord != nil {
		m["ordinal"] = *ord
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

func sessionMeta(id string, parent, fork, provider *string, ownStart *uint64, source any) map[string]any {
	m := map[string]any{
		"id": id, "session_id": id,
		"timestamp":  "2026-11-18T10:00:00Z",
		"cwd":        "/home/u/projects/demo",
		"originator": "user", "cli_version": "0.154.0",
	}
	if parent != nil {
		m["parent_thread_id"] = *parent
	}
	if fork != nil {
		m["forked_from_id"] = *fork
	}
	if provider != nil {
		m["model_provider"] = *provider
	}
	if ownStart != nil {
		m["subagent_history_start_ordinal"] = *ownStart
	}
	if source != nil {
		m["source"] = source
	}
	return m
}

func turnContext(turn *string, model string) map[string]any {
	m := map[string]any{"model": model, "cwd": "/home/u"}
	if turn != nil {
		m["turn_id"] = *turn
	}
	return m
}

func threadSettings(model, provider string) map[string]any {
	return map[string]any{"type": "thread_settings_applied", "thread_settings": map[string]any{"model": model, "model_provider_id": provider}}
}

func tokenCount() map[string]any {
	return map[string]any{"type": "token_count"}
}

func compacted(checkpoint map[string]any) map[string]any {
	return map[string]any{"message": "compacted", "latest_token_usage_record": checkpoint}
}

func usage(input, cached int64, cw *int64, output, reasoning, total int64) map[string]any {
	m := map[string]any{
		"input_tokens": input, "cached_input_tokens": cached,
		"output_tokens": output, "reasoning_output_tokens": reasoning, "total_tokens": total,
	}
	if cw != nil {
		m["cache_write_input_tokens"] = *cw
	}
	return m
}

func record(respID, threadID, turnID string, u map[string]any) map[string]any {
	copy := make(map[string]any, len(u)+1)
	for k, v := range u {
		copy[k] = v
	}
	return map[string]any{
		"thread_id": threadID, "turn_id": turnID,
		"session_id": threadID, "root_turn_id": turnID,
		"response_id": respID, "usage": copy,
		"turn_token_usage": copy, "thread_token_usage": copy,
	}
}

type fileBuilder struct {
	lines [][]byte
}

func newFile() *fileBuilder { return &fileBuilder{} }

func (b *fileBuilder) add(tstamp *string, ord *uint64, typ string, payload any) *fileBuilder {
	b.lines = append(b.lines, envLine(tstamp, ord, typ, payload))
	return b
}

func (b *fileBuilder) raw(s string) *fileBuilder {
	b.lines = append(b.lines, []byte(s))
	return b
}

func (b *fileBuilder) write(t *testing.T, root, rel string) string {
	return b.writeRaw(t, root, rel, append(bytes.Join(b.lines, []byte("\n")), '\n'))
}

func (b *fileBuilder) writeZst(t *testing.T, root, rel string) string {
	t.Helper()
	var z bytes.Buffer
	zw, err := zstd.NewWriter(&z)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(append(bytes.Join(b.lines, []byte("\n")), '\n')); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.writeRaw(t, root, rel, z.Bytes())
}

func (b *fileBuilder) writeRaw(t *testing.T, root, rel string, data []byte) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func scanRoot(t *testing.T, root string, window Window) Preview {
	t.Helper()
	pv, err := Scan(context.Background(), root, window)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return pv
}

func recIDs(recs []Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.ID
	}
	return out
}

func wantExcluded(t *testing.T, pv Preview, want map[string]int) {
	t.Helper()
	if !reflect.DeepEqual(pv.Excluded, want) {
		t.Errorf("Excluded = %v, want %v", pv.Excluded, want)
	}
}

func wantIDs(t *testing.T, pv Preview, want []string) {
	t.Helper()
	if got := recIDs(pv.Records); !slices.Equal(got, want) {
		t.Errorf("Records ids = %v, want %v", got, want)
	}
}

func findRec(t *testing.T, pv Preview, id string) Record {
	t.Helper()
	for _, r := range pv.Records {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("record %q not found in %v", id, recIDs(pv.Records))
	return Record{}
}

func TestScanActiveArchivedMixed(t *testing.T) {
	root := t.TempDir()
	active := newFile().
		add(ts("2026-11-17T00:00:00.000Z"), u64p(0), "session_meta", sessionMeta("ta-001", nil, nil, strp("openai"), nil, "cli")).
		add(ts("2026-11-17T00:00:00.000Z"), nil, "token_usage_record", record("resp_444", "ta-001", "turn-4", usage(10, 0, nil, 10, 0, 20))).
		add(ts("2026-11-18T10:00:01.000Z"), u64p(1), "turn_context", turnContext(strp("turn-1"), "gpt-5-mini")).
		add(ts("2026-11-18T10:00:05.000Z"), u64p(2), "token_usage_record", record("resp_111", "ta-001", "turn-1", usage(1200, 300, i64p(50), 800, 200, 2000))).
		add(ts("2026-11-18T10:00:06.000Z"), u64p(3), "event_msg", threadSettings("gpt-5", "openai")).
		add(ts("2026-11-18T10:01:00.000Z"), u64p(4), "token_usage_record", record("resp_222", "ta-001", "turn-2", usage(500, 0, nil, 200, 0, 700))).
		add(ts("2026-11-18T10:01:01.000Z"), u64p(5), "compacted", compacted(record("resp_dummy", "ta-001", "turn-2", usage(9999, 0, nil, 9999, 0, 19998)))).
		add(ts("2026-11-18T10:01:02.000Z"), u64p(6), "event_msg", tokenCount()).
		raw("not-json{{").
		add(ts("2026-11-18T10:02:00.000Z"), u64p(7), "world_state", map[string]any{"full": true}).
		add(ts("2026-11-19T00:00:00.000Z"), u64p(8), "token_usage_record", record("resp_333", "ta-001", "turn-3", usage(10, 0, nil, 10, 0, 20))).
		add(ts("2026-11-18T10:03:00.000Z"), u64p(10), "token_usage_record", record("resp_555", "tb-999", "turn-1", usage(10, 0, nil, 10, 0, 20))).
		add(nil, u64p(11), "token_usage_record", record("resp_666", "ta-001", "turn-5", usage(10, 0, nil, 10, 0, 20))).
		add(ts("2026-11-18T25:00:00.000Z"), u64p(12), "token_usage_record", record("resp_777", "ta-001", "turn-6", usage(10, 0, nil, 10, 0, 20)))
	active.write(t, root, "sessions/2026/11/18/rollout-2026-11-18T10-00-00-ab12cd34.jsonl")

	archived := newFile().
		add(ts("2026-11-17T09:00:00.000Z"), u64p(0), "session_meta", sessionMeta("tb-002", nil, nil, strp("openai"), nil, "cli")).
		add(ts("2026-11-17T09:05:00.000Z"), u64p(1), "token_usage_record", record("resp_888", "tb-002", "turn-1", usage(400, 0, nil, 120, 0, 520))).
		add(ts("2026-11-19T00:00:00.000Z"), u64p(2), "token_usage_record", record("resp_889", "tb-002", "turn-2", usage(40, 0, nil, 40, 0, 80)))
	archived.writeZst(t, root, "archived_sessions/rollout-2026-11-17T09-00-00-ef56gh78.jsonl.zst")

	pv := scanRoot(t, root, testWindow)
	wantIDs(t, pv, []string{"resp_444", "resp_888", "resp_111", "resp_222"})
	wantExcluded(t, pv, map[string]int{
		ExclLegacy:           1,
		ExclMalformedJSON:    1,
		ExclOutsideWindow:    2,
		ExclForeignThread:    1,
		ExclMissingTimestamp: 1,
		ExclInvalidTimestamp: 1,
	})

	r111 := findRec(t, pv, "resp_111")
	if r111.Provider != "openai" || r111.Model != "gpt-5-mini" || r111.SessionID != "ta-001" {
		t.Errorf("resp_111 attribution = %+v", r111)
	}
	if r111.Input != 850 || r111.Output != 800 || r111.CacheRead != 300 || r111.CacheWrite != 50 {
		t.Errorf("resp_111 tokens = %+v", r111)
	}
	if r111.Subagent == nil || *r111.Subagent {
		t.Errorf("resp_111 subagent = %v, want false", r111.Subagent)
	}
	r222 := findRec(t, pv, "resp_222")
	if r222.Model != "gpt-5" || r222.Input != 500 || r222.Output != 200 {
		t.Errorf("resp_222 = %+v", r222)
	}
	r444 := findRec(t, pv, "resp_444")
	if r444.Model != unknownModel || r444.Input != 10 || r444.Output != 10 {
		t.Errorf("resp_444 = %+v", r444)
	}
	r888 := findRec(t, pv, "resp_888")
	if r888.SessionID != "tb-002" || r888.Input != 400 || r888.Output != 120 {
		t.Errorf("resp_888 = %+v", r888)
	}
}

func TestScanZeroWindow(t *testing.T) {
	root := t.TempDir()
	newFile().
		add(ts("2026-11-18T10:00:00.000Z"), u64p(0), "session_meta", sessionMeta("ta-001", nil, nil, strp("openai"), nil, "cli")).
		add(ts("2026-11-18T10:00:05.000Z"), u64p(1), "token_usage_record", record("resp_111", "ta-001", "turn-1", usage(1200, 300, i64p(50), 800, 200, 2000))).
		add(ts("2026-11-18T10:01:00.000Z"), u64p(2), "token_usage_record", record("resp_222", "ta-001", "turn-2", usage(500, 0, nil, 200, 0, 700))).
		add(ts("2026-11-18T10:02:00.000Z"), u64p(3), "event_msg", tokenCount()).
		raw("not-json{{").
		add(ts("2026-11-18T10:03:00.000Z"), u64p(4), "token_usage_record", record("resp_333", "ta-001", "turn-3", usage(10, 0, nil, 10, 0, 20))).
		add(ts("2026-11-17T00:00:00.000Z"), u64p(5), "token_usage_record", record("resp_444", "ta-001", "turn-4", usage(10, 0, nil, 10, 0, 20))).
		write(t, root, "sessions/2026/11/18/rollout-2026-11-18T10-00-00-ab12cd34.jsonl")

	pv := scanRoot(t, root, Window{})
	wantIDs(t, pv, nil)
	wantExcluded(t, pv, map[string]int{
		ExclNoWindow:      4,
		ExclLegacy:        1,
		ExclMalformedJSON: 1,
	})
}
func TestScanForkSubagentOwnership(t *testing.T) {
	root := t.TempDir()
	newFile().
		add(ts("2026-11-18T11:00:00.000Z"), u64p(1), "session_meta", sessionMeta("ta-parent", nil, nil, strp("openai"), nil, "cli")).
		add(ts("2026-11-18T11:00:01.000Z"), u64p(2), "session_meta", sessionMeta("tc-child", strp("ta-parent"), nil, strp("openai"), u64p(100), map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": "ta-parent"}}})).
		add(ts("2026-11-18T11:00:30.000Z"), u64p(30), "turn_context", turnContext(strp("turn-1"), "gpt-6")).
		add(ts("2026-11-18T11:00:40.000Z"), u64p(40), "event_msg", threadSettings("gpt-6", "openai")).
		add(ts("2026-11-18T11:00:50.000Z"), u64p(50), "token_usage_record", record("resp_child_early", "tc-child", "turn-1", usage(10, 0, nil, 10, 0, 20))).
		add(ts("2026-11-18T11:02:00.000Z"), u64p(200), "token_usage_record", record("resp_child_200", "tc-child", "turn-2", usage(20, 0, nil, 20, 0, 40))).
		write(t, root, "sessions/2026/11/18/rollout-2026-11-18T11-00-00-child0001.jsonl")

	pv := scanRoot(t, root, testWindow)
	wantIDs(t, pv, []string{"resp_child_200"})
	wantExcluded(t, pv, map[string]int{ExclInheritedPrefix: 1})
	r := findRec(t, pv, "resp_child_200")
	if r.SessionID != "tc-child" || r.ParentSessionID != "ta-parent" {
		t.Errorf("child session/parent = %+v", r)
	}
	if r.Subagent == nil || !*r.Subagent {
		t.Errorf("child subagent = %v, want true", r.Subagent)
	}
	if r.Model != unknownModel {
		t.Errorf("child model = %q, want unknown (inherited context ignored)", r.Model)
	}
}

func TestScanUnrelatedMetaNotAdopted(t *testing.T) {
	root := t.TempDir()
	newFile().
		add(ts("2026-11-18T10:00:00.000Z"), u64p(0), "session_meta", sessionMeta("ta-001", nil, nil, strp("openai"), nil, "cli")).
		add(ts("2026-11-18T10:01:00.000Z"), u64p(1), "session_meta", sessionMeta("tx-999", nil, nil, strp("openai"), nil, "cli")).
		add(ts("2026-11-18T10:02:00.000Z"), u64p(2), "token_usage_record", record("resp_keep", "ta-001", "turn-1", usage(10, 0, nil, 10, 0, 20))).
		add(ts("2026-11-18T10:03:00.000Z"), u64p(3), "token_usage_record", record("resp_foreign", "tx-999", "turn-2", usage(20, 0, nil, 20, 0, 40))).
		write(t, root, "sessions/2026/11/18/rollout-unrelated.jsonl")

	pv := scanRoot(t, root, testWindow)
	wantIDs(t, pv, []string{"resp_keep"})
	wantExcluded(t, pv, map[string]int{ExclForeignThread: 1})
}

func TestScanPlainSiblingPreferred(t *testing.T) {
	root := t.TempDir()
	newFile().
		add(ts("2026-11-18T10:00:00.000Z"), u64p(0), "session_meta", sessionMeta("ta-001", nil, nil, strp("openai"), nil, "cli")).
		add(ts("2026-11-18T10:00:05.000Z"), u64p(1), "token_usage_record", record("resp_A", "ta-001", "turn-1", usage(10, 0, nil, 10, 0, 20))).
		write(t, root, "sessions/2026/11/18/rollout-a.jsonl")
	newFile().
		add(ts("2026-11-18T10:00:00.000Z"), u64p(0), "session_meta", sessionMeta("tb-002", nil, nil, strp("openai"), nil, "cli")).
		add(ts("2026-11-18T10:00:05.000Z"), u64p(1), "token_usage_record", record("resp_B", "tb-002", "turn-1", usage(20, 0, nil, 20, 0, 40))).
		writeZst(t, root, "sessions/2026/11/18/rollout-a.jsonl.zst")

	pv := scanRoot(t, root, testWindow)
	wantIDs(t, pv, []string{"resp_A"})
	wantExcluded(t, pv, map[string]int{})
}

func TestScanConflictsAndDedup(t *testing.T) {
	root := t.TempDir()
	newFile().
		add(ts("2026-11-18T10:00:00.000Z"), u64p(0), "session_meta", sessionMeta("ta-001", nil, nil, nil, nil, "cli")).
		add(ts("2026-11-18T10:00:05.000Z"), u64p(1), "token_usage_record", record("resp_X", "ta-001", "turn-1", usage(100, 0, nil, 50, 0, 150))).
		add(ts("2026-11-18T10:01:05.000Z"), u64p(2), "token_usage_record", record("resp_Y", "ta-001", "turn-2", usage(100, 0, nil, 50, 0, 150))).
		add(ts("2026-11-18T10:02:05.000Z"), u64p(3), "token_usage_record", record("resp_Z", "ta-001", "turn-3", usage(100, 0, nil, 50, 0, 150))).
		add(ts("2026-11-18T10:03:05.000Z"), u64p(4), "token_usage_record", record("resp_W", "ta-001", "turn-4", usage(10, 0, nil, 10, 0, 20))).
		add(ts("2026-11-18T10:04:05.000Z"), u64p(5), "token_usage_record", record("resp_TS", "ta-001", "turn-5", usage(30, 0, nil, 15, 0, 45))).
		write(t, root, "sessions/2026/11/18/rollout-a.jsonl")
	// A copied archive keeps the same thread id and timestamp: identical
	// accounting is a duplicate, not a conflict; unknown provider metadata is
	// enriched. A differing timestamp for the same response is a conflict.
	newFile().
		add(ts("2026-11-18T10:00:00.000Z"), u64p(0), "session_meta", sessionMeta("ta-001", nil, nil, strp("openai"), nil, "cli")).
		add(ts("2026-11-18T10:01:05.000Z"), u64p(2), "token_usage_record", record("resp_Y", "ta-001", "turn-2", usage(100, 0, nil, 50, 0, 150))).
		add(ts("2026-11-18T10:02:05.000Z"), u64p(3), "token_usage_record", record("resp_Z", "ta-001", "turn-3", usage(100, 0, nil, 50, 0, 150))).
		add(ts("2026-11-18T11:00:00.000Z"), u64p(6), "token_usage_record", record("resp_TS", "ta-001", "turn-5", usage(30, 0, nil, 15, 0, 45))).
		write(t, root, "sessions/2026/11/18/rollout-b.jsonl")
	// Different session or different accounting for the same response id is a
	// conflict: both copies are withheld.
	newFile().
		add(ts("2026-11-18T10:00:00.000Z"), u64p(0), "session_meta", sessionMeta("tc-001", nil, nil, strp("openai"), nil, "cli")).
		add(ts("2026-11-18T10:00:05.000Z"), u64p(1), "token_usage_record", record("resp_X", "tc-001", "turn-1", usage(101, 0, nil, 51, 0, 152))).
		add(ts("2026-11-18T10:03:05.000Z"), u64p(4), "token_usage_record", record("resp_W", "tc-001", "turn-4", usage(10, 0, nil, 10, 0, 20))).
		write(t, root, "sessions/2026/11/18/rollout-c.jsonl")

	pv := scanRoot(t, root, testWindow)
	wantIDs(t, pv, []string{"resp_Y", "resp_Z"})
	wantExcluded(t, pv, map[string]int{ExclConflict: 3, ExclDuplicate: 2})
	if got := findRec(t, pv, "resp_Z"); got.Provider != "openai" {
		t.Errorf("resp_Z provider = %q, want openai (enriched from unknown)", got.Provider)
	}
}

func TestScanMalformedUsageMatrix(t *testing.T) {
	root := t.TempDir()
	missingInput := usage(0, 0, nil, 10, 0, 10)
	delete(missingInput, "input_tokens")
	nullCW := usage(100, 0, nil, 50, 0, 150)
	nullCW["cache_write_input_tokens"] = nil
	newFile().
		add(ts("2026-11-18T10:00:00.000Z"), u64p(0), "session_meta", sessionMeta("tm-001", nil, nil, strp("openai"), nil, "cli")).
		add(ts("2026-11-18T10:00:01.000Z"), u64p(1), "token_usage_record", record("resp_m1", "tm-001", "turn-1", missingInput)).
		add(ts("2026-11-18T10:00:02.000Z"), u64p(2), "token_usage_record", record("resp_m2", "tm-001", "turn-2", usage(100, 200, nil, 50, 0, 150))).
		add(ts("2026-11-18T10:00:03.000Z"), u64p(3), "token_usage_record", record("resp_m3", "tm-001", "turn-3", nullCW)).
		add(ts("2026-11-18T10:00:04.000Z"), u64p(4), "token_usage_record", record("resp_m4", "tm-001", "turn-4", usage(100, 0, nil, 50, 0, 200))).
		add(ts("2026-11-18T10:00:05.000Z"), u64p(5), "token_usage_record", record("resp_m5", "tm-001", "turn-5", usage(100, 0, nil, 50, 60, 150))).
		add(ts("2026-11-18T10:00:06.000Z"), u64p(6), "token_usage_record", record("resp_z", "tm-001", "turn-6", usage(0, 0, nil, 0, 0, 0))).
		add(ts("2026-11-18T10:00:07.000Z"), u64p(7), "token_usage_record", record("resp_b", "tm-001", "turn-7", usage(2_000_000_000, 0, nil, 0, 0, 2_000_000_000))).
		write(t, root, "sessions/2026/11/18/rollout-m.jsonl")

	pv := scanRoot(t, root, testWindow)
	wantIDs(t, pv, nil)
	wantExcluded(t, pv, map[string]int{
		ExclMalformedUsage: 5,
		ExclAllZero:        1,
		ExclBounds:         1,
	})

	rootNoMeta := t.TempDir()
	newFile().
		add(ts("2026-11-18T10:00:00.000Z"), u64p(0), "token_usage_record", record("resp_orphan", "tn-001", "turn-1", usage(10, 0, nil, 10, 0, 20))).
		write(t, rootNoMeta, "sessions/2026/11/18/rollout-nometa.jsonl")
	pvNoMeta := scanRoot(t, rootNoMeta, testWindow)
	wantIDs(t, pvNoMeta, nil)
	wantExcluded(t, pvNoMeta, map[string]int{ExclMissingThread: 1})
}

func TestScanMunchContradiction(t *testing.T) {
	root := t.TempDir()
	newFile().
		add(ts("2026-11-16T10:00:00.000Z"), u64p(0), "session_meta", sessionMeta("tm-001", nil, nil, strp("munch_chatgpt"), nil, "cli")).
		add(ts("2026-11-16T10:00:05.000Z"), nil, "token_usage_record", record("resp_prior", "tm-001", "turn-0", usage(25, 0, nil, 25, 0, 50))).
		add(ts("2026-11-18T10:00:05.000Z"), u64p(1), "token_usage_record", record("resp_M", "tm-001", "turn-1", usage(100, 0, nil, 50, 0, 150))).
		write(t, root, "sessions/2026/11/18/rollout-munch.jsonl")
	newFile().
		add(ts("2026-11-18T10:00:00.000Z"), u64p(0), "session_meta", sessionMeta("to-001", nil, nil, strp("openai"), nil, "cli")).
		add(ts("2026-11-18T10:00:05.000Z"), u64p(1), "token_usage_record", record("resp_O", "to-001", "turn-1", usage(50, 0, nil, 25, 0, 75))).
		write(t, root, "sessions/2026/11/18/rollout-openai.jsonl")

	pv := scanRoot(t, root, testWindow)
	wantIDs(t, pv, nil)
	wantExcluded(t, pv, map[string]int{
		ExclWindowContradiction:   2,
		ExclWithheldContradiction: 1,
	})
}

func TestScanMunchAfterWindowNoContradiction(t *testing.T) {
	root := t.TempDir()
	newFile().
		add(ts("2026-11-19T10:00:00.000Z"), u64p(0), "session_meta", sessionMeta("tm-001", nil, nil, strp("munch_chatgpt"), nil, "cli")).
		add(ts("2026-11-19T10:00:05.000Z"), u64p(1), "token_usage_record", record("resp_M", "tm-001", "turn-1", usage(100, 0, nil, 50, 0, 150))).
		write(t, root, "archived_sessions/rollout-munch.jsonl")
	newFile().
		add(ts("2026-11-18T10:00:00.000Z"), u64p(0), "session_meta", sessionMeta("to-001", nil, nil, strp("openai"), nil, "cli")).
		add(ts("2026-11-18T10:00:05.000Z"), u64p(1), "token_usage_record", record("resp_O", "to-001", "turn-1", usage(50, 0, nil, 25, 0, 75))).
		add(ts("2026-11-18T10:00:06.000Z"), u64p(2), "token_usage_record", record("resp_M", "to-001", "turn-2", usage(100, 0, nil, 50, 0, 150))).
		write(t, root, "sessions/2026/11/18/rollout-openai.jsonl")

	pv := scanRoot(t, root, testWindow)
	wantIDs(t, pv, []string{"resp_O"})
	wantExcluded(t, pv, map[string]int{ExclOverlapMunch: 2})
}
func TestScanProxiedDropRemovesEligibleCopy(t *testing.T) {
	root := t.TempDir()
	newFile().
		add(ts("2026-11-17T10:00:00.000Z"), u64p(0), "session_meta", sessionMeta("to-001", nil, nil, strp("openai"), nil, "cli")).
		add(ts("2026-11-18T10:00:05.000Z"), u64p(1), "token_usage_record", record("resp_K", "to-001", "turn-1", usage(10, 0, nil, 10, 0, 20))).
		write(t, root, "sessions/2026/11/15/rollout-a.jsonl")
	newFile().
		add(ts("2026-11-19T10:00:00.000Z"), u64p(0), "session_meta", sessionMeta("to-001", nil, nil, strp("munch_chatgpt"), nil, "cli")).
		add(ts("2026-11-19T10:00:05.000Z"), u64p(1), "token_usage_record", record("resp_K", "to-001", "turn-1", usage(10, 0, nil, 10, 0, 20))).
		write(t, root, "sessions/2026/11/19/rollout-b.jsonl")

	pv := scanRoot(t, root, testWindow)
	wantIDs(t, pv, nil)
	wantExcluded(t, pv, map[string]int{ExclOverlapMunch: 2})
}

func TestScanEligibleVsPostWindowConflict(t *testing.T) {
	root := t.TempDir()
	newFile().
		add(ts("2026-11-18T10:00:00.000Z"), u64p(0), "session_meta", sessionMeta("ta-001", nil, nil, strp("openai"), nil, "cli")).
		add(ts("2026-11-18T10:00:05.000Z"), u64p(1), "token_usage_record", record("resp_PW", "ta-001", "turn-1", usage(10, 0, nil, 10, 0, 20))).
		add(ts("2026-11-19T00:00:00.000Z"), u64p(2), "token_usage_record", record("resp_PW", "ta-001", "turn-1", usage(11, 0, nil, 11, 0, 22))).
		write(t, root, "sessions/2026/11/18/rollout-pw.jsonl")

	pv := scanRoot(t, root, testWindow)
	wantIDs(t, pv, nil)
	wantExcluded(t, pv, map[string]int{ExclOutsideWindow: 1, ExclConflict: 1})
}

func TestScanUnsafeIdentifiers(t *testing.T) {
	root := t.TempDir()
	newFile().
		add(ts("2026-11-18T10:00:00.000Z"), u64p(0), "session_meta", sessionMeta("ta-001", nil, nil, strp("openai"), nil, "cli")).
		add(ts("2026-11-18T10:00:01.000Z"), u64p(1), "token_usage_record", record("a/b", "ta-001", "turn-1", usage(10, 0, nil, 10, 0, 20))).
		add(ts("2026-11-18T10:00:02.000Z"), u64p(2), "token_usage_record", record("resp_2", "ta\n2", "turn-2", usage(10, 0, nil, 10, 0, 20))).
		write(t, root, "sessions/2026/11/18/rollout-u1.jsonl")
	newFile().
		add(ts("2026-11-18T10:00:00.000Z"), u64p(0), "session_meta", sessionMeta("ta-002", nil, nil, strp("evil/provider"), nil, "cli")).
		add(ts("2026-11-18T10:00:01.000Z"), u64p(1), "token_usage_record", record("resp_3", "ta-002", "turn-1", usage(10, 0, nil, 10, 0, 20))).
		write(t, root, "sessions/2026/11/18/rollout-u2.jsonl")

	pv := scanRoot(t, root, testWindow)
	wantIDs(t, pv, nil)
	wantExcluded(t, pv, map[string]int{ExclUnsafeID: 3})
}

func TestScanIncompleteAndOversizedLines(t *testing.T) {
	root := t.TempDir()
	f := newFile().
		add(ts("2026-11-18T10:00:00.000Z"), u64p(0), "session_meta", sessionMeta("ta-001", nil, nil, strp("openai"), nil, "cli")).
		add(ts("2026-11-18T10:00:05.000Z"), u64p(1), "token_usage_record", record("resp_ok", "ta-001", "turn-1", usage(10, 0, nil, 10, 0, 20)))
	f.writeRaw(t, root, "sessions/2026/11/18/rollout-tail.jsonl", append(bytes.Join(f.lines, []byte("\n")), []byte("\npartial-line-without-newline")...))

	pv := scanRoot(t, root, testWindow)
	wantIDs(t, pv, []string{"resp_ok"})
	wantExcluded(t, pv, map[string]int{ExclIncompleteLine: 1})

	root2 := t.TempDir()
	newFile().
		add(ts("2026-11-18T10:00:00.000Z"), u64p(0), "session_meta", sessionMeta("ta-001", nil, nil, strp("openai"), nil, "cli")).
		add(ts("2026-11-18T10:00:05.000Z"), u64p(1), "token_usage_record", record("resp_ok", "ta-001", "turn-1", usage(10, 0, nil, 10, 0, 20))).
		raw(strings.Repeat("a", maxLineBytes+1)).
		write(t, root2, "sessions/2026/11/18/rollout-big.jsonl")

	pv2 := scanRoot(t, root2, testWindow)
	wantIDs(t, pv2, []string{"resp_ok"})
	wantExcluded(t, pv2, map[string]int{ExclOversizedLine: 1})
}

func TestScanReadFailedZstdGarbage(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "archived_sessions", "rollout-garbage.jsonl.zst")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("this is not a zstd frame"), 0o600); err != nil {
		t.Fatal(err)
	}
	pv := scanRoot(t, root, testWindow)
	wantIDs(t, pv, nil)
	wantExcluded(t, pv, map[string]int{ExclReadFailed: 1})
}

func TestScanDeterministic(t *testing.T) {
	root := t.TempDir()
	newFile().
		add(ts("2026-11-18T10:00:00.000Z"), u64p(0), "session_meta", sessionMeta("ta-001", nil, nil, strp("openai"), nil, "cli")).
		add(ts("2026-11-18T10:00:05.000Z"), u64p(1), "token_usage_record", record("resp_A", "ta-001", "turn-1", usage(10, 0, nil, 10, 0, 20))).
		add(ts("2026-11-18T10:01:05.000Z"), u64p(2), "token_usage_record", record("resp_B", "ta-001", "turn-2", usage(20, 0, nil, 20, 0, 40))).
		write(t, root, "sessions/2026/11/18/rollout-a.jsonl")
	newFile().
		add(ts("2026-11-18T10:00:00.000Z"), u64p(0), "session_meta", sessionMeta("ta-001", nil, nil, strp("openai"), nil, "cli")).
		add(ts("2026-11-18T10:00:05.000Z"), u64p(1), "token_usage_record", record("resp_A", "ta-001", "turn-1", usage(10, 0, nil, 10, 0, 20))).
		write(t, root, "sessions/2026/11/18/rollout-b.jsonl")

	first := scanRoot(t, root, testWindow)
	second := scanRoot(t, root, testWindow)
	if !reflect.DeepEqual(first, second) {
		t.Errorf("scans differ:\nfirst: %+v\nsecond: %+v", first, second)
	}
	wantExcluded(t, first, map[string]int{ExclDuplicate: 1})
}

func TestScanEmptyAndMissingRoot(t *testing.T) {
	empty := t.TempDir()
	pv := scanRoot(t, empty, testWindow)
	wantIDs(t, pv, nil)
	wantExcluded(t, pv, map[string]int{})

	// An empty root string is the missing default Codex home: empty success.
	if pv, err := Scan(context.Background(), "", testWindow); err != nil {
		t.Errorf("Scan on empty root: want no error, got %v", err)
	} else if len(pv.Records) != 0 || len(pv.Excluded) != 0 {
		t.Errorf("Scan on empty root = %+v, want empty", pv)
	}

	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := Scan(context.Background(), missing, testWindow); err == nil {
		t.Errorf("Scan on missing root: want error, got nil")
	}
}

func TestScanDegenerateWindow(t *testing.T) {
	root := t.TempDir()
	newFile().
		add(ts("2026-11-18T10:00:00.000Z"), u64p(0), "session_meta", sessionMeta("ta-001", nil, nil, strp("openai"), nil, "cli")).
		add(ts("2026-11-18T10:00:05.000Z"), u64p(1), "token_usage_record", record("resp_A", "ta-001", "turn-1", usage(10, 0, nil, 10, 0, 20))).
		write(t, root, "sessions/2026/11/18/rollout-a.jsonl")

	degenerate := Window{From: winFrom, Before: winFrom}
	pv := scanRoot(t, root, degenerate)
	wantIDs(t, pv, nil)
	wantExcluded(t, pv, map[string]int{ExclOutsideWindow: 1})
}

func TestScanFixtureRoot(t *testing.T) {
	root := filepath.Join("testdata")
	pv := scanRoot(t, root, Window{From: time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC), Before: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)})
	wantIDs(t, pv, []string{"resp_10002", "resp_10001"})
	wantExcluded(t, pv, map[string]int{ExclLegacy: 1})
	r := findRec(t, pv, "resp_10001")
	if r.Model != "gpt-5-mini" || r.Provider != "openai" || r.Input != 850 || r.Output != 800 {
		t.Errorf("resp_10001 = %+v", r)
	}
}
