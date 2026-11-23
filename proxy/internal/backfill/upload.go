package backfill

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Wire protocol constants mirroring supabase/functions/contributions/validation.ts
// and the retained publish_contributions RPC.
const (
	HarnessClaude = "claude-code"
	HarnessCodex  = "codex"

	maxObservationsPerChunk = 500        // server MAX_OBSERVATIONS
	maxChunkBytes           = 256 * 1024 // server MAX_BODY_BYTES
	maxChunks               = 1000       // server MAX_CHUNKS
	maxResponseBytes        = 16 * 1024
	requestTimeout          = 10 * time.Second
	maxRateLimitWaits       = 12
)

// Errors are generic by design: they never carry URLs, paths, API keys, IDs,
// timestamps, or raw server bodies.
var (
	errUnsupportedHarness   = errors.New("unsupported backfill harness")
	errInvalidClaudeID      = errors.New("Claude records require request and response IDs")
	errNoAPIKey             = errors.New("API key not configured")
	errInvalidURL           = errors.New("ingest URL rejected: must be absolute https pointing at /functions/v1/ingest (http only for numeric loopback)")
	errDuplicateCanonicalID = errors.New("records contain duplicate canonical IDs")
	errTooManyChunks        = errors.New("records exceed the maximum number of chunks")
	errChunkTooLarge        = errors.New("a single record exceeds the maximum chunk size")
	errWindowConflict       = errors.New("source state is already registered with a different window")
	errStateCorrupt         = errors.New("source state is unreadable")
	errUnauthorized         = errors.New("contributions request failed: unauthorized")
	errProtocol             = errors.New("contributions request failed: rejected by server")
	errConflict             = errors.New("contributions request failed: conflict")
	errRateLimited          = errors.New("contributions request failed: rate limited")
	errServer               = errors.New("contributions request failed: server error")
	errNetwork              = errors.New("contributions request failed: network error")
	errAck                  = errors.New("contributions acknowledgment missing")
	errCanceled             = errors.New("contributions request canceled")
	errTimedOut             = errors.New("contributions request timed out")
)

// Upload publishes records to the TokenMunchers contributions endpoint derived
// from the configured /functions/v1/ingest URL. It implements the staged
// begin/chunk/commit contract:
//
//   - records are sorted by canonical ID defensively and encoded once as
//     observations with the typed allowlist the server requires (unknown cost
//     as null, empty session/parent as null);
//   - rows are partitioned into deterministic chunks bounded by <=500
//     observations and the 256 KiB body budget (envelope reserved exactly);
//   - the snapshot ID is a stable content hash of the window, the source
//     device, and the encoded chunk boundaries, so unchanged reruns replay the
//     exact same staged snapshot while any change (archive or layout) stages a
//     new one;
//   - interruption and ambiguous responses are safe: the server's digest and
//     idempotent commit receipt converge on the original accepted count;
//   - 429 rate limits wait only on a valid Retry-After (1..60s), supporting
//     >58 chunks; a staging-limit 429 (no header) returns immediately; auth
//     and protocol errors are never retried; network and server errors abort
//     the run for an explicit rerun (no destructive cancel of staging);
//   - a private source state per root and harness stores a random device ID and
//     the immutable exact window under stateDir, generated once race-safely.
//
// It returns the server's accepted count (rows inserted in the original
// commit, preserved on idempotent replay). Empty records neither mutate state
// nor touch the network.
func Upload(ctx context.Context, harness, ingestURL, apiKey, stateDir, root string, window Window, records []Record) (int, error) {
	if harness != HarnessCodex && harness != HarnessClaude {
		return 0, errUnsupportedHarness
	}
	if len(records) == 0 {
		return 0, nil
	}
	if apiKey == "" {
		return 0, errNoAPIKey
	}
	contribURL, err := deriveContributionsURL(ingestURL)
	if err != nil {
		return 0, err
	}

	sorted := make([]Record, len(records))
	copy(sorted, records)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for i := 1; i < len(sorted); i++ {
		if sorted[i].ID == sorted[i-1].ID {
			return 0, errDuplicateCanonicalID
		}
	}

	rows := make([]json.RawMessage, len(sorted))
	for i, r := range sorted {
		if harness == HarnessClaude && (!validClaudeWireID(r.ID, "req_") || !validClaudeWireID(r.ResponseID, "msg_")) {
			return 0, errInvalidClaudeID
		}
		rows[i], err = json.Marshal(encodeObservation(harness, r))
		if err != nil {
			return 0, errProtocol
		}
	}

	state, err := loadOrCreateSource(stateDir, harness, root, window)
	if err != nil {
		return 0, err
	}

	chunks, err := splitRows(harness, rows)
	if err != nil {
		return 0, err
	}
	snapshotID := snapshotUUID(state.DeviceID, window, chunks)

	client := newClient()

	beginBody, err := json.Marshal(beginRequest{
		Action:       "begin",
		DeviceID:     state.DeviceID,
		Harness:      harness,
		SnapshotID:   snapshotID,
		BackfillFrom: wireTime(window.From),
		Cutoff:       wireTime(window.Before),
		ChunkCount:   len(chunks),
	})
	if err != nil {
		return 0, errProtocol
	}
	st, _, rb, err := sendWithRetry(ctx, client, contribURL, apiKey, beginBody)
	if err != nil {
		return 0, err
	}
	if err := checkAck("begin", snapshotID, st, rb); err != nil {
		return 0, err
	}

	for i, rows := range chunks {
		body, err := chunkBody(harness, state.DeviceID, snapshotID, i, rows)
		if err != nil {
			return 0, errProtocol
		}
		if len(body) > maxChunkBytes {
			return 0, errChunkTooLarge
		}
		st, _, rb, err = sendWithRetry(ctx, client, contribURL, apiKey, body)
		if err != nil {
			return 0, err
		}
		if err := checkAck("chunk", snapshotID, st, rb); err != nil {
			return 0, err
		}
	}

	commitBody, err := json.Marshal(commitRequest{
		Action:     "commit",
		DeviceID:   state.DeviceID,
		Harness:    harness,
		SnapshotID: snapshotID,
	})
	if err != nil {
		return 0, errProtocol
	}
	st, _, rb, err = sendWithRetry(ctx, client, contribURL, apiKey, commitBody)
	if err != nil {
		return 0, err
	}
	return checkCommitAck(snapshotID, len(records), st, rb)
}

// deriveContributionsURL validates the ingest URL under the same telemetry
// destination policy the live reporter uses (absolute https, or http only for
// numeric loopback; no userinfo, query, or fragment) and derives the sibling
// POST endpoint by replacing the trailing /functions/v1/ingest path segment.
func deriveContributionsURL(ingestURL string) (string, error) {
	if ingestURL == "" || strings.ContainsAny(ingestURL, "?#") {
		return "", errInvalidURL
	}
	u, err := url.Parse(ingestURL)
	if err != nil || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Host == "" {
		return "", errInvalidURL
	}
	host := u.Hostname()
	if host == "" {
		return "", errInvalidURL
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return "", errInvalidURL
		}
	default:
		return "", errInvalidURL
	}
	path := strings.TrimSuffix(u.Path, "/")
	if !strings.HasSuffix(path, "/functions/v1/ingest") {
		return "", errInvalidURL
	}
	u.Path = strings.TrimSuffix(path, "ingest") + "contributions"
	u.RawPath = ""
	return u.String(), nil
}

// newClient never follows redirects so the bearer token and payload cannot be
// forwarded to another destination.
func newClient() *http.Client {
	return &http.Client{
		Timeout: requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func sendOnce(ctx context.Context, c *http.Client, url, apiKey string, body []byte) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, errProtocol
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := c.Do(req)
	if err != nil {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return 0, nil, nil, errTimedOut
		case errors.Is(ctx.Err(), context.Canceled):
			return 0, nil, nil, errCanceled
		default:
			return 0, nil, nil, errNetwork
		}
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, nil, errNetwork
	}
	return resp.StatusCode, resp.Header, rb, nil
}

// sendWithRetry sends one request, retrying only 429 rate limits that carry a
// valid Retry-After (1..60s). A staging-limit 429 without the header returns
// immediately; auth, protocol, and server errors are returned as-is; an
// explicit rerun covers network failures.
func sendWithRetry(ctx context.Context, c *http.Client, url, apiKey string, body []byte) (int, http.Header, []byte, error) {
	for waits := 0; ; {
		st, hdr, rb, err := sendOnce(ctx, c, url, apiKey, body)
		if err != nil {
			return st, hdr, rb, err
		}
		if st == http.StatusTooManyRequests {
			if d, ok := retryAfter(hdr, rb); ok && waits < maxRateLimitWaits {
				if !sleepCtx(ctx, d) {
					return st, hdr, rb, ctx.Err()
				}
				waits++
				continue
			}
		}
		return st, hdr, rb, nil
	}
}

// retryAfter reports the server-sanctioned wait for a 429. Only a valid
// Retry-After (header or body, 1..60 seconds) authorizes waiting; a staging
// limit 429 carries none and must return immediately.
func retryAfter(hdr http.Header, rb []byte) (time.Duration, bool) {
	secs := 0
	if v := hdr.Get("Retry-After"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			secs = n
		}
	}
	var body struct {
		RetryAfterSeconds *int `json:"retry_after_seconds"`
	}
	_ = json.Unmarshal(rb, &body)
	if body.RetryAfterSeconds != nil && secs == 0 {
		secs = *body.RetryAfterSeconds
	}
	if secs < 1 || secs > 60 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func statusError(st int) error {
	switch {
	case st >= 200 && st < 300:
		return nil
	case st == http.StatusUnauthorized:
		return fmt.Errorf("%w (HTTP %d)", errUnauthorized, st)
	case st == http.StatusTooManyRequests:
		return fmt.Errorf("%w (HTTP %d)", errRateLimited, st)
	case st == http.StatusConflict:
		return fmt.Errorf("%w (HTTP %d)", errConflict, st)
	case st >= 400 && st < 500:
		return fmt.Errorf("%w (HTTP %d)", errProtocol, st)
	default:
		return fmt.Errorf("%w (HTTP %d)", errServer, st)
	}
}

// checkAck verifies a begin/chunk acknowledgment: 2xx plus ok:true and an
// exact action and snapshot ID match. A mismatch is ambiguous; the caller may
// rerun safely.
func checkAck(wantAction, wantSnapshot string, st int, rb []byte) error {
	if err := statusError(st); err != nil {
		return err
	}
	var ack struct {
		OK         bool   `json:"ok"`
		Action     string `json:"action"`
		SnapshotID string `json:"snapshot_id"`
	}
	if err := json.Unmarshal(rb, &ack); err != nil || !ack.OK || ack.Action != wantAction || ack.SnapshotID != wantSnapshot {
		return errAck
	}
	return nil
}

// checkCommitAck extracts the accepted count from the commit receipt. The
// server returns the count inserted in the original commit, including on
// idempotent replay; it must fall within the number of records sent.
func checkCommitAck(wantSnapshot string, maxAccepted int, st int, rb []byte) (int, error) {
	if err := statusError(st); err != nil {
		return 0, err
	}
	var ack struct {
		OK         bool   `json:"ok"`
		Action     string `json:"action"`
		SnapshotID string `json:"snapshot_id"`
		Committed  bool   `json:"committed"`
		Accepted   *int   `json:"accepted"`
	}
	if err := json.Unmarshal(rb, &ack); err != nil || !ack.OK || ack.Action != "commit" || ack.SnapshotID != wantSnapshot || !ack.Committed || ack.Accepted == nil {
		return 0, errAck
	}
	n := *ack.Accepted
	if n < 0 || n > maxAccepted {
		return 0, errAck
	}
	return n, nil
}

type beginRequest struct {
	Action       string `json:"action"`
	DeviceID     string `json:"device_id"`
	Harness      string `json:"harness"`
	SnapshotID   string `json:"snapshot_id"`
	BackfillFrom string `json:"backfill_from"`
	Cutoff       string `json:"cutoff"`
	ChunkCount   int    `json:"chunk_count"`
}

type chunkRequest struct {
	Action                 string            `json:"action"`
	DeviceID               string            `json:"device_id"`
	Harness                string            `json:"harness"`
	SnapshotID             string            `json:"snapshot_id"`
	Index                  int               `json:"index"`
	Observations           []json.RawMessage `json:"observations"`
	SupersededCanonicalIDs []string          `json:"superseded_canonical_ids"`
	IdentityAliases        []any             `json:"identity_aliases"`
}

type commitRequest struct {
	Action     string `json:"action"`
	DeviceID   string `json:"device_id"`
	Harness    string `json:"harness"`
	SnapshotID string `json:"snapshot_id"`
}

// wireObservation is the typed allowlist of one observation. ResponseID is
// only emitted for Claude backfill; unknown fields reject the whole chunk.
type wireObservation struct {
	Harness          string   `json:"harness"`
	CanonicalID      string   `json:"canonical_id"`
	ResponseID       string   `json:"response_id,omitempty"`
	IdentityStrength string   `json:"identity_strength"`
	Grain            string   `json:"grain"`
	Timestamp        string   `json:"timestamp"`
	IntervalStart    *string  `json:"interval_start"`
	IntervalEnd      *string  `json:"interval_end"`
	Provider         string   `json:"provider"`
	Model            string   `json:"model"`
	SessionID        *string  `json:"session_id"`
	ParentSessionID  *string  `json:"parent_session_id"`
	IsSubagent       *bool    `json:"is_subagent"`
	InputTokens      int64    `json:"input_tokens"`
	OutputTokens     int64    `json:"output_tokens"`
	CacheReadTokens  int64    `json:"cache_read_tokens"`
	CacheWriteTokens int64    `json:"cache_write_tokens"`
	CallCount        int      `json:"call_count"`
	CostUSD          *float64 `json:"cost_usd"`
	CostBasis        string   `json:"cost_basis"`
	Quality          string   `json:"quality"`
}

func wireTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func encodeObservation(harness string, r Record) wireObservation {
	o := wireObservation{
		Harness:          harness,
		CanonicalID:      r.ID,
		IdentityStrength: "response",
		Grain:            "request",
		Timestamp:        wireTime(r.Timestamp),
		Provider:         r.Provider,
		Model:            r.Model,
		IsSubagent:       r.Subagent,
		InputTokens:      r.Input,
		OutputTokens:     r.Output,
		CacheReadTokens:  r.CacheRead,
		CacheWriteTokens: r.CacheWrite,
		CallCount:        1,
		CostUSD:          nil,
		CostBasis:        "unknown",
		Quality:          "observed",
	}
	if harness == HarnessClaude {
		o.ResponseID = r.ResponseID
		o.IdentityStrength = "request"
	} else {
		o.SessionID = strPtr(r.SessionID)
		o.ParentSessionID = strPtr(r.ParentSessionID)
	}
	return o
}

func validClaudeWireID(id, prefix string) bool {
	if len(id) <= len(prefix) || len(id) > 128 || !strings.HasPrefix(id, prefix) {
		return false
	}
	for i := len(prefix); i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '/' && prefix == "req_") {
			return false
		}
	}
	return true
}

// splitRows partitions encoded observations into deterministic chunks bounded
// by <=500 rows and the 256 KiB body budget. The exact envelope size (with the
// widest possible index and full-width UUIDs) is reserved up front and every
// row adds its encoded length plus one comma, so a generated chunk body never
// exceeds the budget. The partitioning depends only on the sorted row set.
func splitRows(harness string, rows []json.RawMessage) ([][]json.RawMessage, error) {
	base := chunkEnvelopeSize(harness)
	budget := maxChunkBytes - base
	var groups [][]json.RawMessage
	var cur []json.RawMessage
	curBytes := 0
	for _, row := range rows {
		add := len(row) + 1 // comma
		if add > budget {
			return nil, errChunkTooLarge
		}
		if len(cur) > 0 && (len(cur)+1 > maxObservationsPerChunk || curBytes+add > budget) {
			groups = append(groups, cur)
			cur = nil
			curBytes = 0
		}
		cur = append(cur, row)
		curBytes += add
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	if len(groups) > maxChunks {
		return nil, errTooManyChunks
	}
	return groups, nil
}

// chunkEnvelopeSize returns the exact encoded size of a chunk envelope with an
// empty observation array, the widest possible index, and the device and
// snapshot UUIDs every real chunk carries. Every row adds len(row)+1 bytes on
// top of this.
func chunkEnvelopeSize(harness string) int {
	const uuidWidth = "00000000-0000-0000-0000-000000000000"
	b, err := json.Marshal(chunkRequest{
		Action:                 "chunk",
		DeviceID:               uuidWidth,
		Harness:                harness,
		SnapshotID:             uuidWidth,
		Index:                  maxChunks - 1,
		Observations:           []json.RawMessage{},
		SupersededCanonicalIDs: []string{},
		IdentityAliases:        []any{},
	})
	if err != nil {
		return 0
	}
	return len(b)
}

func chunkBody(harness, deviceID, snapshotID string, index int, rows []json.RawMessage) ([]byte, error) {
	return json.Marshal(chunkRequest{
		Action:                 "chunk",
		DeviceID:               deviceID,
		Harness:                harness,
		SnapshotID:             snapshotID,
		Index:                  index,
		Observations:           rows,
		SupersededCanonicalIDs: []string{},
		IdentityAliases:        []any{},
	})
}

// snapshotUUID derives a stable content-based snapshot ID from the source
// device (random UUID), the window, and the encoded chunk boundaries. It never
// involves the root path or the API key, so key rotation is safe, unchanged
// reruns replay the exact same staged snapshot, and any content or layout
// change stages a new snapshot that cannot alias an old one.
func snapshotUUID(deviceID string, window Window, chunks [][]json.RawMessage) string {
	h := sha256.New()
	io.WriteString(h, "codex-backfill:v1:")
	io.WriteString(h, deviceID)
	h.Write([]byte{0})
	io.WriteString(h, wireTime(window.From))
	h.Write([]byte{0})
	io.WriteString(h, wireTime(window.Before))
	h.Write([]byte{0})
	for _, chunk := range chunks {
		for _, row := range chunk {
			h.Write(row)
			h.Write([]byte{0})
		}
		h.Write([]byte{0xff}) // chunk boundary marker
	}
	return stableUUIDv5(hex.EncodeToString(h.Sum(nil)))
}

var uuidNamespace = [16]byte{0x6b, 0xa7, 0xb8, 0x11, 0xe0, 0x4f, 0x4f, 0x8c, 0x9a, 0x33, 0x8f, 0x5b, 0x7d, 0x10, 0x2c, 0x09}

func fmtUUID(b [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func stableUUIDv5(name string) string {
	h := sha1.New()
	h.Write(uuidNamespace[:])
	h.Write([]byte(name))
	var b [16]byte
	copy(b[:], h.Sum(nil)[:16])
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return fmtUUID(b)
}

func randomUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmtUUID(b), nil
}

type windowState struct {
	Version  int    `json:"version"`
	DeviceID string `json:"device_id"`
	From     string `json:"from"`
	Before   string `json:"before"`
}

const stateVersion = 1

func statePath(stateDir, harness, root string) string {
	sum := sha256.Sum256([]byte(root))
	prefix := "backfill-source-"
	if harness == HarnessClaude {
		prefix += HarnessClaude + "-"
	}
	return filepath.Join(stateDir, prefix+hex.EncodeToString(sum[:])+".json")
}

func readState(path string) (windowState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return windowState{}, err
	}
	var st windowState
	if err := json.Unmarshal(b, &st); err != nil || st.DeviceID == "" || st.Version != stateVersion {
		return windowState{}, errStateCorrupt
	}
	return st, nil
}

// loadOrCreateSource loads the private per-root, per-harness source state, creating it
// with a fresh random device id and the exact window on first use. Creation is
// race-safe (atomic link with exclusive semantics) so concurrent first uses
// converge on one device id. The window is immutable per source; a different
// window is rejected before any network work.
func loadOrCreateSource(stateDir, harness, root string, window Window) (windowState, error) {
	path := statePath(stateDir, harness, root)
	from, before := wireTime(window.From), wireTime(window.Before)

	if st, err := readState(path); err == nil {
		if st.From != from || st.Before != before {
			return windowState{}, errWindowConflict
		}
		return st, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return windowState{}, err
	}

	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return windowState{}, errStateCorrupt
	}
	id, err := randomUUIDv4()
	if err != nil {
		return windowState{}, errStateCorrupt
	}
	st := windowState{Version: stateVersion, DeviceID: id, From: from, Before: before}
	tmp, err := os.CreateTemp(stateDir, "backfill-state-*")
	if err != nil {
		return windowState{}, errStateCorrupt
	}
	defer os.Remove(tmp.Name())
	payload, err := json.Marshal(st)
	if err != nil {
		tmp.Close()
		return windowState{}, errStateCorrupt
	}
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return windowState{}, errStateCorrupt
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return windowState{}, errStateCorrupt
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return windowState{}, errStateCorrupt
	}
	if err := tmp.Close(); err != nil {
		return windowState{}, errStateCorrupt
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			winner, err2 := readState(path)
			if err2 != nil {
				return windowState{}, errStateCorrupt
			}
			if winner.From != from || winner.Before != before {
				return windowState{}, errWindowConflict
			}
			return winner, nil
		}
		return windowState{}, errStateCorrupt
	}
	return st, nil
}
