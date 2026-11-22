// Package backfill scans retained OpenAI Codex rollout files for observed,
// per-response token usage, returning metadata-only records for preview or
// upload. It understands the rollout JSONL wire format of openai/codex at
// pinned commit 6562117a81a9457c9d67a4d15b884bf80e7805f7.
//
// Only envelope fields and explicit metadata are read; conversation payloads
// are never decoded. Discovery covers <root>/sessions and
// <root>/archived_sessions (rollout-*.jsonl[.zst]), never follows symlinks,
// and prefers a plain .jsonl over its compressed sibling. A record is kept
// only when its thread_id matches the rollout's own session_meta id, its
// disjoint token buckets validate against the source totals, and it lies in
// [From, Before). Missing provider/model stay literally "unknown"; missing
// parent/subagent knowledge stays nil. Observed munch_chatgpt usage before
// the exclusive cutoff contradicts the attested pre-proxy window.
package backfill

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Exclusion keys count what the scan deliberately does not include.
const (
	ExclLegacy                = "legacy"
	ExclMalformedJSON         = "malformed_json"
	ExclOversizedLine         = "oversized_line"
	ExclIncompleteLine        = "incomplete_line"
	ExclReadFailed            = "read_failed"
	ExclMalformedEnvelope     = "malformed_envelope"
	ExclMalformedUsage        = "malformed_usage"
	ExclMissingIdentity       = "missing_identity"
	ExclUnsafeID              = "unsafe_id"
	ExclMissingThread         = "missing_thread"
	ExclForeignThread         = "foreign_thread"
	ExclInheritedPrefix       = "inherited_prefix"
	ExclAllZero               = "all_zero"
	ExclBounds                = "bounds"
	ExclConflict              = "conflict"
	ExclDuplicate             = "duplicate"
	ExclOverlapMunch          = "overlap_munch"
	ExclDesktopCollected      = "desktop_collected"
	ExclWindowContradiction   = "window_contradiction"
	ExclWithheldContradiction = "withheld_contradiction"
	ExclMissingTimestamp      = "missing_timestamp"
	ExclInvalidTimestamp      = "invalid_timestamp"
	ExclOutsideWindow         = "outside_window"
	ExclNoWindow              = "no_window"
)

const (
	unknownProvider      = "unknown"
	unknownModel         = "unknown"
	munchChatgptProvider = "munch_chatgpt"

	maxResponseIDLen = 512
	maxProviderLen   = 64
	maxModelLen      = 128
	maxSessionLen    = 128
	maxBucket        = 1_000_000_000
)

// maxLineBytes bounds a single line; longer lines are oversized exclusions.
const maxLineBytes = 8 << 20

// Window defines the scan interval, inclusive of From and exclusive of Before.
// A zero Window means no verified interval exists.
type Window struct {
	From   time.Time
	Before time.Time
}

// IsZero reports whether the window is the zero value.
func (w Window) IsZero() bool { return w.From.IsZero() && w.Before.IsZero() }

// DesktopOwnership reserves IDs already recorded by Munch's Codex desktop
// collector, and timestamps at or after its current activation when enabled.
type DesktopOwnership struct {
	Since       time.Time
	RecordedIDs map[string]struct{}
}

// Record is one observed, completed response: metadata only, never content.
// SessionID is the record's thread_id; ParentSessionID is the owning thread's
// parent (or fork origin) when known. Subagent is nil when unknown.
type Record struct {
	ID              string    `json:"id"`
	Provider        string    `json:"provider"`
	Model           string    `json:"model"`
	SessionID       string    `json:"session_id"`
	ParentSessionID string    `json:"parent_session_id,omitempty"`
	Timestamp       time.Time `json:"timestamp"`
	Subagent        *bool     `json:"subagent,omitempty"`
	Input           int64     `json:"input_tokens"`
	Output          int64     `json:"output_tokens"`
	CacheRead       int64     `json:"cache_read_tokens"`
	CacheWrite      int64     `json:"cache_write_tokens"`
}

// Preview is the scan result: accepted records plus exclusion counts.
type Preview struct {
	Records  []Record
	Excluded map[string]int
}

// Scan returns the preview for window. An empty root (missing default Codex
// home) is an empty successful preview. A zero window excludes modern
// candidates as no_window while still counting legacy/diagnostic exclusions.
func Scan(ctx context.Context, root string, window Window, desktop DesktopOwnership) (Preview, error) {
	if root == "" {
		return Preview{Excluded: map[string]int{}}, nil
	}
	paths, err := discover(ctx, root)
	if err != nil {
		return Preview{}, err
	}
	excl := make(map[string]int)
	acct := &accounting{byID: make(map[string]*acctEntry), conflicts: make(map[string]bool)}
	guard := &windowGuard{proxied: make(map[string]bool), desktop: desktop}
	for _, rel := range paths {
		if err := ctx.Err(); err != nil {
			return Preview{}, err
		}
		scanFile(ctx, root, rel, window, excl, acct, guard)
	}
	if guard.contradicted {
		kept := 0
		for _, e := range acct.byID {
			if e.eligible {
				kept++
			}
		}
		excl[ExclWithheldContradiction] += kept
		acct = &accounting{byID: make(map[string]*acctEntry), conflicts: make(map[string]bool)}
	}
	return Preview{Records: acct.records(), Excluded: excl}, nil
}

var rolloutNameRe = regexp.MustCompile(`^rollout-.*\.jsonl(\.zst)?$`)

func discover(ctx context.Context, root string) ([]string, error) {
	li, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("backfill: root %q: %w", root, err)
	}
	if li.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("backfill: root %q is a symlink", root)
	}
	if !li.IsDir() {
		return nil, fmt.Errorf("backfill: root %q is not a directory", root)
	}
	var paths []string
	for _, sub := range []string{"sessions", "archived_sessions"} {
		base := filepath.Join(root, sub)
		bi, err := os.Lstat(base)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("backfill: %q: %w", base, err)
		}
		if bi.Mode()&fs.ModeSymlink != 0 || !bi.IsDir() {
			continue
		}
		err = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if d.Type()&fs.ModeSymlink != 0 {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() || !d.Type().IsRegular() || !rolloutNameRe.MatchString(d.Name()) {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			paths = append(paths, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("backfill: walking %q: %w", base, err)
		}
	}
	// Upstream sibling precedence: a plain .jsonl hides its compressed sibling.
	plain := make(map[string]bool, len(paths))
	for _, p := range paths {
		if strings.HasSuffix(p, ".jsonl") {
			plain[p] = true
		}
	}
	out := paths[:0]
	for _, p := range paths {
		if strings.HasSuffix(p, ".zst") && plain[strings.TrimSuffix(p, ".zst")] {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func scanFile(ctx context.Context, root, rel string, window Window, excl map[string]int, acct *accounting, guard *windowGuard) {
	path := filepath.Join(root, filepath.FromSlash(rel))
	f, err := os.Open(path)
	if err != nil {
		excl[ExclReadFailed]++
		return
	}
	defer f.Close()
	var src io.Reader = f
	if strings.HasSuffix(rel, ".zst") {
		zr, err := zstd.NewReader(f,
			zstd.WithDecoderMaxWindow(64<<20),
			zstd.WithDecoderConcurrency(2))
		if err != nil {
			excl[ExclReadFailed]++
			return
		}
		defer zr.Close()
		src = zr
	}
	lr := &lineReader{r: bufio.NewReaderSize(src, 64<<10)}
	st := &fileState{modelByTurn: make(map[string]string)}
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		raw, status := lr.next(maxLineBytes)
		switch status {
		case lineOversize:
			excl[ExclOversizedLine]++
			continue
		case lineIncomplete:
			excl[ExclIncompleteLine]++
			return
		case lineReadError:
			excl[ExclReadFailed]++
			return
		case lineEOF:
			return
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		consumeLine(raw, st, window, excl, acct, guard)
	}
}

type lineStatus int

const (
	lineOK lineStatus = iota
	lineOversize
	lineIncomplete
	lineReadError
	lineEOF
)

type lineReader struct {
	r *bufio.Reader
}

// next returns the next line (without its trailing newline or \r), or a
// terminal status. Short lines are returned as zero-copy slices of the stale
// buffer; longer lines accumulate up to max, after which the rest is drained
// without being buffered.
func (lr *lineReader) next(max int) ([]byte, lineStatus) {
	frag, err := lr.r.ReadSlice('\n')
	if err == nil {
		line := frag[:len(frag)-1]
		if n := len(line); n > 0 && line[n-1] == '\r' {
			line = line[:n-1]
		}
		if len(line) > max {
			return nil, lineOversize
		}
		return line, lineOK
	}
	if err == bufio.ErrBufferFull {
		buf := append([]byte(nil), frag...)
		for len(buf) <= max {
			frag, err = lr.r.ReadSlice('\n')
			if err == nil {
				buf = append(buf, frag...)
				line := buf[:len(buf)-1]
				if n := len(line); n > 0 && line[n-1] == '\r' {
					line = line[:n-1]
				}
				if len(line) > max {
					return nil, lineOversize
				}
				return line, lineOK
			}
			if err == bufio.ErrBufferFull {
				buf = append(buf, frag...)
				continue
			}
			if err == io.EOF {
				return nil, lineIncomplete
			}
			return nil, lineReadError
		}
		for {
			if _, derr := lr.r.ReadSlice('\n'); derr == nil || derr != bufio.ErrBufferFull {
				return nil, lineOversize
			}
		}
	}
	if err == io.EOF {
		if len(frag) > 0 {
			return nil, lineIncomplete
		}
		return nil, lineEOF
	}
	return nil, lineReadError
}

// wireLine is the rollout envelope for the item types the scanner decodes.
// The payload is copied only for these relevant types; transcript payloads
// are never captured.
type wireLine struct {
	Timestamp string          `json:"timestamp"`
	Ordinal   json.RawMessage `json:"ordinal"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

func parseOrdinal(raw json.RawMessage) (uint64, bool) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, false
	}
	var v uint64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, false
	}
	return v, true
}

func consumeLine(raw []byte, st *fileState, window Window, excl map[string]int, acct *accounting, guard *windowGuard) {
	// Type-first decode: unknown or irrelevant item types are ignored without
	// ever decoding their payloads.
	var typOnly struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &typOnly); err != nil {
		excl[ExclMalformedJSON]++
		return
	}
	switch typOnly.Type {
	case "session_meta", "turn_context", "event_msg", "token_usage_record":
		var env wireLine
		if err := json.Unmarshal(raw, &env); err != nil || len(env.Payload) == 0 {
			excl[ExclMalformedEnvelope]++
			return
		}
		ordinal, ordOK := parseOrdinal(env.Ordinal)
		switch env.Type {
		case "session_meta":
			handleMeta(&env, ordinal, ordOK, st, excl)
		case "turn_context":
			handleTurnContext(&env, ordinal, ordOK, st, excl)
		case "event_msg":
			handleEventMsg(&env, ordinal, ordOK, st, excl)
		case "token_usage_record":
			handleRecord(&env, ordinal, ordOK, st, window, excl, acct, guard)
		}
	case "":
		excl[ExclMalformedEnvelope]++
	case "compacted":
		// Embedded checkpoint copies are not responses; ignore entirely.
	}
}

// fileState carries the metadata context of one rollout file.
type fileState struct {
	thread      string
	parent      string
	fork        string
	provider    string
	model       string
	subagent    *bool
	haveOwn     bool
	ownStart    uint64
	modelByTurn map[string]string
}

// windowGuard tracks fail-closed window evidence across all files.
type windowGuard struct {
	desktop      DesktopOwnership
	collected    map[string]bool
	contradicted bool
	proxied      map[string]bool
}

type metaPayload struct {
	ID                          string          `json:"id"`
	ParentThreadID              *string         `json:"parent_thread_id"`
	ForkedFromID                *string         `json:"forked_from_id"`
	ModelProvider               *string         `json:"model_provider"`
	SubagentHistoryStartOrdinal *uint64         `json:"subagent_history_start_ordinal"`
	Source                      json.RawMessage `json:"source"`
}

func handleMeta(env *wireLine, ordinal uint64, ordOK bool, st *fileState, excl map[string]int) {
	var p metaPayload
	if json.Unmarshal(env.Payload, &p) != nil || p.ID == "" {
		excl[ExclMalformedEnvelope]++
		return
	}
	inherited := st.haveOwn && ordOK && ordinal < st.ownStart
	if st.thread == "" || p.ID == st.thread {
		st.applyMeta(p)
		return
	}
	// Replayed lineage metadata (parent/fork ids or inherited ordinal) is not
	// this rollout's identity. A descendant boundary (meta declaring the
	// current thread as its parent) is the file's true owner. Anything else is
	// an unrelated meta and never replaces the first owning meta.
	if inherited || p.ID == st.parent || p.ID == st.fork {
		return
	}
	if p.ParentThreadID != nil && *p.ParentThreadID == st.thread {
		st.adopt(p)
	}
}

func (st *fileState) adopt(p metaPayload) {
	st.model = unknownModel
	st.modelByTurn = make(map[string]string)
	st.applyMeta(p)
}

func (st *fileState) applyMeta(p metaPayload) {
	parent := ""
	if p.ParentThreadID != nil {
		parent = *p.ParentThreadID
	}
	if parent == "" {
		if sp, ok := spawnParentID(p.Source); ok {
			parent = sp
		}
	}
	st.thread = p.ID
	st.parent = parent
	st.fork = ""
	if p.ForkedFromID != nil {
		st.fork = *p.ForkedFromID
	}
	if p.ModelProvider != nil {
		st.provider = *p.ModelProvider
	}
	if p.SubagentHistoryStartOrdinal != nil {
		st.haveOwn = true
		st.ownStart = *p.SubagentHistoryStartOrdinal
	}
	st.subagent = classifySubagent(p, parent)
}

func classifySubagent(p metaPayload, parent string) *bool {
	if parent != "" || sourceHasSubagent(p.Source) {
		b := true
		return &b
	}
	if len(p.Source) > 0 {
		var s string
		if json.Unmarshal(p.Source, &s) == nil {
			b := false
			return &b
		}
	}
	return nil
}

func sourceHasSubagent(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	_, ok := m["subagent"]
	if !ok {
		_, ok = m["sub_agent"]
	}
	return ok
}

func spawnParentID(raw json.RawMessage) (string, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return "", false
	}
	for _, key := range []string{"subagent", "sub_agent"} {
		sub, ok := m[key]
		if !ok {
			continue
		}
		var sm map[string]json.RawMessage
		if json.Unmarshal(sub, &sm) != nil {
			continue
		}
		spawn, ok := sm["thread_spawn"]
		if !ok {
			continue
		}
		var sp struct {
			ParentThreadID *string `json:"parent_thread_id"`
		}
		if json.Unmarshal(spawn, &sp) == nil && sp.ParentThreadID != nil {
			return *sp.ParentThreadID, true
		}
	}
	return "", false
}

type turnContextPayload struct {
	TurnID *string `json:"turn_id"`
	Model  string  `json:"model"`
}

func handleTurnContext(env *wireLine, ordinal uint64, ordOK bool, st *fileState, excl map[string]int) {
	if st.haveOwn && ordOK && ordinal < st.ownStart {
		return // inherited parent context
	}
	var p turnContextPayload
	if json.Unmarshal(env.Payload, &p) != nil || p.Model == "" {
		excl[ExclMalformedEnvelope]++
		return
	}
	if p.TurnID != nil && *p.TurnID != "" {
		st.modelByTurn[*p.TurnID] = p.Model
		return
	}
	st.model = p.Model
}

type threadSettingsPayload struct {
	Model           string `json:"model"`
	ModelProviderID string `json:"model_provider_id"`
}

type eventMsgPayload struct {
	Type           string                 `json:"type"`
	ThreadSettings *threadSettingsPayload `json:"thread_settings"`
}

func handleEventMsg(env *wireLine, ordinal uint64, ordOK bool, st *fileState, excl map[string]int) {
	var p eventMsgPayload
	if json.Unmarshal(env.Payload, &p) != nil {
		excl[ExclMalformedEnvelope]++
		return
	}
	switch p.Type {
	case "token_count":
		excl[ExclLegacy]++
	case "thread_settings_applied":
		if st.haveOwn && ordOK && ordinal < st.ownStart {
			return // inherited parent settings
		}
		if p.ThreadSettings != nil {
			if p.ThreadSettings.Model != "" {
				st.model = p.ThreadSettings.Model
			}
			if p.ThreadSettings.ModelProviderID != "" {
				st.provider = p.ThreadSettings.ModelProviderID
			}
		}
	}
}

type usageFields struct {
	InputTokens           *int64          `json:"input_tokens"`
	CachedInputTokens     *int64          `json:"cached_input_tokens"`
	CacheWriteInputTokens json.RawMessage `json:"cache_write_input_tokens"`
	OutputTokens          *int64          `json:"output_tokens"`
	ReasoningOutputTokens *int64          `json:"reasoning_output_tokens"`
	TotalTokens           *int64          `json:"total_tokens"`
}

type recordPayload struct {
	ThreadID   string       `json:"thread_id"`
	TurnID     string       `json:"turn_id"`
	ResponseID string       `json:"response_id"`
	Usage      *usageFields `json:"usage"`
}

type tokenDeltas struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// tokensFor validates the pinned wire invariants and converts inclusive source
// input into disjoint buckets. cache_write_input_tokens is serde-zeroed only
// when absent; null or malformed present values fail.
func tokensFor(u *usageFields) (tokenDeltas, bool) {
	var t tokenDeltas
	if u == nil || u.InputTokens == nil || u.CachedInputTokens == nil ||
		u.OutputTokens == nil || u.ReasoningOutputTokens == nil || u.TotalTokens == nil {
		return t, false
	}
	input, cached := *u.InputTokens, *u.CachedInputTokens
	output, reasoning, total := *u.OutputTokens, *u.ReasoningOutputTokens, *u.TotalTokens
	if input < 0 || cached < 0 || output < 0 || reasoning < 0 || total < 0 {
		return t, false
	}
	var cw int64
	if len(u.CacheWriteInputTokens) > 0 {
		if bytes.Equal(u.CacheWriteInputTokens, []byte("null")) {
			return t, false
		}
		if err := json.Unmarshal(u.CacheWriteInputTokens, &cw); err != nil || cw < 0 {
			return t, false
		}
	}
	if cw > input || cached > input-cw {
		return t, false
	}
	if reasoning > output {
		return t, false
	}
	if input > math.MaxInt64-output || input+output != total {
		return t, false
	}
	t.Input = input - cached - cw
	t.CacheRead = cached
	t.CacheWrite = cw
	t.Output = output
	return t, true
}

func handleRecord(env *wireLine, ordinal uint64, ordOK bool, st *fileState, window Window, excl map[string]int, acct *accounting, guard *windowGuard) {
	var p recordPayload
	if json.Unmarshal(env.Payload, &p) != nil {
		excl[ExclMalformedEnvelope]++
		return
	}
	if p.ResponseID == "" || p.ThreadID == "" {
		excl[ExclMissingIdentity]++
		return
	}
	if !safeID(p.ResponseID, maxResponseIDLen) || !safeID(p.ThreadID, maxSessionLen) {
		excl[ExclUnsafeID]++
		return
	}
	if st.thread == "" {
		excl[ExclMissingThread]++
		return
	}
	if p.ThreadID != st.thread {
		excl[ExclForeignThread]++
		return
	}
	if st.haveOwn && (!ordOK || ordinal < st.ownStart) {
		excl[ExclInheritedPrefix]++
		return
	}
	tokens, ok := tokensFor(p.Usage)
	if !ok {
		excl[ExclMalformedUsage]++
		return
	}
	if tokens.Input > maxBucket || tokens.Output > maxBucket ||
		tokens.CacheRead > maxBucket || tokens.CacheWrite > maxBucket {
		excl[ExclBounds]++
		return
	}
	if tokens.Input == 0 && tokens.Output == 0 && tokens.CacheRead == 0 && tokens.CacheWrite == 0 {
		excl[ExclAllZero]++
		return
	}
	ts, present, valid := parseRolloutTime(env.Timestamp)
	if !present {
		excl[ExclMissingTimestamp]++
		return
	}
	if !valid {
		excl[ExclInvalidTimestamp]++
		return
	}
	model := st.model
	if p.TurnID != "" {
		if m, ok := st.modelByTurn[p.TurnID]; ok {
			model = m
		}
	}
	if model == "" {
		model = unknownModel
	}
	provider := st.provider
	if provider == "" {
		provider = unknownProvider
	}
	if !guard.collected[p.ResponseID] {
		_, recorded := guard.desktop.RecordedIDs[p.ResponseID]
		if recorded || (!guard.desktop.Since.IsZero() && !ts.Before(guard.desktop.Since)) {
			if guard.collected == nil {
				guard.collected = make(map[string]bool)
			}
			guard.collected[p.ResponseID] = true
			excl[ExclDesktopCollected]++
		}
	}
	// Desktop ownership must not mask evidence contradicting the pre-proxy window.
	if provider == munchChatgptProvider {
		guard.proxied[p.ResponseID] = true
		if prior := acct.byID[p.ResponseID]; prior != nil && prior.eligible {
			excl[ExclOverlapMunch]++
		}
		delete(acct.byID, p.ResponseID)
		// Even usage before From shows this is not a pre-proxy window.
		if !window.IsZero() && ts.Before(window.Before) {
			guard.contradicted = true
			excl[ExclWindowContradiction]++
			return
		}
		excl[ExclOverlapMunch]++
		return
	}
	if guard.collected[p.ResponseID] {
		// A copy with a different timestamp must not reintroduce this ID.
		delete(acct.byID, p.ResponseID)
		return
	}
	if guard.proxied[p.ResponseID] {
		// An eligible copy of a positively proxied response is never allowed.
		excl[ExclOverlapMunch]++
		return
	}
	parent := st.parent
	if parent == "" {
		parent = st.fork
	}
	if !safeID(provider, maxProviderLen) || !safeID(model, maxModelLen) ||
		(parent != "" && !safeID(parent, maxSessionLen)) {
		excl[ExclUnsafeID]++
		return
	}
	// Registration happens regardless of window membership so that an eligible
	// copy is still compared against a post-window copy of the same response.
	eligible := true
	if window.IsZero() {
		eligible = false
		excl[ExclNoWindow]++
	} else if !ts.Before(window.Before) || ts.Before(window.From) {
		eligible = false
		excl[ExclOutsideWindow]++
	}
	acct.add(Record{
		ID:              p.ResponseID,
		Provider:        provider,
		Model:           model,
		SessionID:       p.ThreadID,
		ParentSessionID: parent,
		Timestamp:       ts,
		Subagent:        st.subagent,
		Input:           tokens.Input,
		Output:          tokens.Output,
		CacheRead:       tokens.CacheRead,
		CacheWrite:      tokens.CacheWrite,
	}, eligible, excl)
}

func parseRolloutTime(s string) (t time.Time, present, valid bool) {
	if s == "" {
		return time.Time{}, false, false
	}
	// Codex writes UTC milliseconds; accept whole seconds or 1–3 fractional digits.
	if len(s) != 20 && (len(s) < 22 || len(s) > 24 || s[19] != '.') || s[len(s)-1] != 'Z' {
		return time.Time{}, true, false
	}
	t, err := time.Parse(time.RFC3339, s)
	return t, true, err == nil && t.Unix() >= 0
}

// safeID accepts only the strict identifier alphabet [A-Za-z0-9._:-].
func safeID(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for _, c := range s {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == ':' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// accounting keeps one record per response id, comparing every copy whether
// or not it lies in the window. Conflicting copies are withheld; duplicates
// enrich unknown metadata and are reported.
type acctEntry struct {
	rec      Record
	eligible bool
}

type accounting struct {
	byID      map[string]*acctEntry
	conflicts map[string]bool
}

func (a *accounting) add(rec Record, eligible bool, excl map[string]int) {
	if a.conflicts[rec.ID] {
		return
	}
	prev, dup := a.byID[rec.ID]
	if !dup {
		a.byID[rec.ID] = &acctEntry{rec: rec, eligible: eligible}
		return
	}
	sameAccounting := prev.rec.Input == rec.Input && prev.rec.Output == rec.Output &&
		prev.rec.CacheRead == rec.CacheRead && prev.rec.CacheWrite == rec.CacheWrite &&
		prev.rec.SessionID == rec.SessionID && prev.rec.Timestamp.Equal(rec.Timestamp)
	attrClash := knownDiff(prev.rec.Provider, rec.Provider, unknownProvider) ||
		knownDiff(prev.rec.Model, rec.Model, unknownModel) ||
		subagentClash(prev.rec.Subagent, rec.Subagent) ||
		parentClash(prev.rec.ParentSessionID, rec.ParentSessionID)
	if !sameAccounting || attrClash {
		delete(a.byID, rec.ID)
		a.conflicts[rec.ID] = true
		excl[ExclConflict]++
		return
	}
	excl[ExclDuplicate]++
	if prev.rec.Provider == unknownProvider {
		prev.rec.Provider = rec.Provider
	}
	if prev.rec.Model == unknownModel {
		prev.rec.Model = rec.Model
	}
	if prev.rec.Subagent == nil {
		prev.rec.Subagent = rec.Subagent
	}
	if prev.rec.ParentSessionID == "" {
		prev.rec.ParentSessionID = rec.ParentSessionID
	}
	prev.eligible = prev.eligible || eligible
}

func knownDiff(a, b, unknown string) bool { return a != unknown && b != unknown && a != b }
func parentClash(a, b string) bool        { return a != "" && b != "" && a != b }

func subagentClash(a, b *bool) bool {
	if a == nil || b == nil {
		return false
	}
	return *a != *b
}

func (a *accounting) records() []Record {
	out := make([]Record, 0, len(a.byID))
	for _, e := range a.byID {
		if e.eligible {
			out = append(out, e.rec)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Timestamp.Equal(out[j].Timestamp) {
			return out[i].Timestamp.Before(out[j].Timestamp)
		}
		if out[i].SessionID != out[j].SessionID {
			return out[i].SessionID < out[j].SessionID
		}
		return out[i].ID < out[j].ID
	})
	return out
}
