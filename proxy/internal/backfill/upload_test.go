package backfill

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Minimal fake contributions endpoint. Each test installs its own handler
// closure that receives every request body and writes a canned ack. No
// backend ledger is reimplemented here; the parent's PGlite/CLI E2E owns the
// server contract.
// ---------------------------------------------------------------------------

type fakeContrib struct {
	mu     sync.Mutex
	ts     *httptest.Server
	bodies [][]byte // every contributions request body, in send order
	ingest int      // hits on the sibling /functions/v1/ingest path
	other  int      // hits on any other path (e.g. redirect targets)
}

func newFakeContrib(t *testing.T, handle func(w http.ResponseWriter, body []byte)) *fakeContrib {
	f := &fakeContrib{}
	mux := http.NewServeMux()
	mux.HandleFunc("/functions/v1/contributions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		handle(w, body)
	})
	mux.HandleFunc("/functions/v1/ingest", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.ingest++
		f.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.other++
		f.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	})
	f.ts = httptest.NewServer(mux)
	t.Cleanup(f.ts.Close)
	return f
}

func (f *fakeContrib) url() string { return f.ts.URL + "/functions/v1/ingest" }

func (f *fakeContrib) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func ack(w http.ResponseWriter, status int, format string, args ...any) {
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, format, args...)
}

func parseChunk(body []byte) (index int, obs int, ids []string) {
	var c struct {
		Index        int              `json:"index"`
		Observations []map[string]any `json:"observations"`
	}
	_ = json.Unmarshal(body, &c)
	for _, o := range c.Observations {
		if id, ok := o["canonical_id"].(string); ok {
			ids = append(ids, id)
		}
	}
	return c.Index, len(c.Observations), ids
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

var uploadWindow = Window{
	From:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	Before: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
}

func testRecord(id string) Record {
	return Record{
		ID:              id,
		Provider:        "openai",
		Model:           "gpt-5-codex",
		SessionID:       "thread_123",
		ParentSessionID: "",
		Timestamp:       time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC),
		Input:           1000,
		Output:          200,
		CacheRead:       300,
		CacheWrite:      50,
	}
}

func testRecords(n int) []Record {
	recs := make([]Record, n)
	for i := range recs {
		recs[i] = testRecord(fmt.Sprintf("resp_%04d", i))
	}
	return recs
}

func newStateDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestUploadInterruptedResumeReplaysExactStage pins the transport core: after
// a 500 in the middle of chunk upload, an unchanged rerun replays the byte-
// identical begin and chunks, same device and snapshot, and converges on the
// same accepted receipt; canonical IDs are sent sorted; only the sibling
// contributions endpoint is touched.
func TestUploadInterruptedResumeReplaysExactStage(t *testing.T) {
	recs := testRecords(1200) // 3 chunks of <=500

	var mu sync.Mutex
	attempt := 0
	var perAttempt [][][]byte
	committed := 0
	f := newFakeContrib(t, func(w http.ResponseWriter, body []byte) {
		var req struct {
			Action     string `json:"action"`
			SnapshotID string `json:"snapshot_id"`
			DeviceID   string `json:"device_id"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		defer mu.Unlock()
		if attempt == 0 {
			return // requests before the first Upload call are impossible
		}
		perAttempt[attempt-1] = append(perAttempt[attempt-1], body)
		switch req.Action {
		case "begin":
			ack(w, 200, `{"ok":true,"action":"begin","snapshot_id":"%s"}`, req.SnapshotID)
		case "chunk":
			if attempt == 1 && len(perAttempt[0]) == 3 { // begin + 2 chunks acked
				ack(w, 500, `{"error":"storage_failed"}`)
				return
			}
			ack(w, 200, `{"ok":true,"action":"chunk","snapshot_id":"%s"}`, req.SnapshotID)
		case "commit":
			committed++
			ack(w, 200, `{"ok":true,"action":"commit","snapshot_id":"%s","accepted":1200,"committed":true}`, req.SnapshotID)
		}
	})
	stateDir := newStateDir(t)

	run := func() (int, error) {
		mu.Lock()
		attempt++
		perAttempt = append(perAttempt, nil)
		mu.Unlock()
		return Upload(context.Background(), f.url(), "k", stateDir, "/r", uploadWindow, recs)
	}

	if _, err := run(); !errors.Is(err, errServer) {
		t.Fatalf("interrupted Upload err = %v, want errServer", err)
	}
	mu.Lock()
	if committed != 0 {
		mu.Unlock()
		t.Fatal("commit happened despite interruption")
	}
	first := make([][]byte, len(perAttempt[0]))
	copy(first, perAttempt[0])
	mu.Unlock()

	accepted, err := run()
	if err != nil || accepted != 1200 {
		t.Fatalf("resume Upload: accepted=%d err=%v", accepted, err)
	}
	mu.Lock()
	defer mu.Unlock()
	second := perAttempt[1]

	// Interrupted run saw begin + chunk0 + chunk1, then the 500.
	if len(first) != 3 {
		t.Fatalf("interrupted run requests = %d, want 3", len(first))
	}
	if len(second) != 5 { // begin + 3 chunks + commit
		t.Fatalf("resume requests = %d, want 5", len(second))
	}
	for i := range first {
		if !bytes.Equal(first[i], second[i]) {
			t.Fatalf("resume request %d differs from interrupted run", i)
		}
	}
	if committed != 1 {
		t.Fatalf("commit executed %d times after resume, want 1", committed)
	}
	if f.ingest != 0 {
		t.Fatalf("ingest endpoint hit %d times", f.ingest)
	}
	var ids []string
	for _, body := range second[1:4] {
		_, _, chunkIDs := parseChunk(body)
		ids = append(ids, chunkIDs...)
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatalf("wire canonical ids not sorted")
	}
}

// TestUploadAckMismatchRejected: an ack with the wrong snapshot ID must stop
// the upload (ambiguous replay) and never reach commit.
func TestUploadAckMismatchRejected(t *testing.T) {
	f := newFakeContrib(t, func(w http.ResponseWriter, body []byte) {
		var req struct {
			Action     string `json:"action"`
			SnapshotID string `json:"snapshot_id"`
			Index      int    `json:"index"`
		}
		_ = json.Unmarshal(body, &req)
		switch req.Action {
		case "begin":
			ack(w, 200, `{"ok":true,"action":"begin","snapshot_id":"%s"}`, req.SnapshotID)
		case "chunk":
			ack(w, 200, `{"ok":true,"action":"chunk","snapshot_id":"00000000-0000-0000-0000-000000000000"}`)
		default:
			ack(w, 200, `{"ok":true,"action":"commit","snapshot_id":"%s","accepted":60,"committed":true}`, req.SnapshotID)
		}
	})
	_, err := Upload(context.Background(), f.url(), "k", newStateDir(t), "/r", uploadWindow, testRecords(60))
	if !errors.Is(err, errAck) {
		t.Fatalf("err = %v, want errAck", err)
	}
	if got := f.requestCount(); got != 2 { // begin + one chunk
		t.Fatalf("requests = %d, want 2 (no commit after bogus ack)", got)
	}
}

// TestUploadCommitAcceptedOutOfBounds: a receipt claiming more rows than were
// sent is not trustworthy and must surface as an ack failure.
func TestUploadCommitAcceptedOutOfBounds(t *testing.T) {
	f := newFakeContrib(t, func(w http.ResponseWriter, body []byte) {
		var req struct {
			Action     string `json:"action"`
			SnapshotID string `json:"snapshot_id"`
		}
		_ = json.Unmarshal(body, &req)
		switch req.Action {
		case "begin":
			ack(w, 200, `{"ok":true,"action":"begin","snapshot_id":"%s"}`, req.SnapshotID)
		case "chunk":
			ack(w, 200, `{"ok":true,"action":"chunk","snapshot_id":"%s"}`, req.SnapshotID)
		default:
			ack(w, 200, `{"ok":true,"action":"commit","snapshot_id":"%s","accepted":999999,"committed":true}`, req.SnapshotID)
		}
	})
	_, err := Upload(context.Background(), f.url(), "k", newStateDir(t), "/r", uploadWindow, testRecords(60))
	if !errors.Is(err, errAck) {
		t.Fatalf("err = %v, want errAck", err)
	}
}

// TestChunkBoundaryPackingWithLargeIdentifiers: observations with large
// provider/model values must pack into chunks respecting both the 500-row and
// the exact 256 KiB envelope-reserved byte budget.
func TestChunkBoundaryPackingWithLargeIdentifiers(t *testing.T) {
	var recs []Record
	for i := range 2000 {
		r := testRecord(fmt.Sprintf("resp_big_%04d", i))
		r.Provider = "p"
		r.Model = "m"
		recs = append(recs, r)
	}
	var chunks [][]byte
	f := newFakeContrib(t, func(w http.ResponseWriter, body []byte) {
		var req struct {
			Action     string `json:"action"`
			SnapshotID string `json:"snapshot_id"`
			Index      int    `json:"index"`
		}
		_ = json.Unmarshal(body, &req)
		switch req.Action {
		case "begin":
			ack(w, 200, `{"ok":true,"action":"begin","snapshot_id":"%s"}`, req.SnapshotID)
		case "chunk":
			chunks = append(chunks, body)
			ack(w, 200, `{"ok":true,"action":"chunk","snapshot_id":"%s"}`, req.SnapshotID)
		default:
			ack(w, 200, `{"ok":true,"action":"commit","snapshot_id":"%s","accepted":2000,"committed":true}`, req.SnapshotID)
		}
	})
	accepted, err := Upload(context.Background(), f.url(), "k", newStateDir(t), "/r", uploadWindow, recs)
	if err != nil || accepted != 2000 {
		t.Fatalf("Upload: accepted=%d err=%v", accepted, err)
	}
	if len(chunks) <= 1 {
		t.Fatalf("expected multi-chunk split, got %d chunks", len(chunks))
	}
	for i, body := range chunks {
		if len(body) > maxChunkBytes {
			t.Fatalf("chunk %d body = %d bytes, limit %d", i, len(body), maxChunkBytes)
		}
		if _, n, _ := parseChunk(body); n > maxObservationsPerChunk {
			t.Fatalf("chunk %d observations = %d, limit %d", i, n, maxObservationsPerChunk)
		}
	}
}

// TestChunkBodiesFitServerLimitWithRealisticRows: realistic rows fill chunks
// by bytes rather than by count, so every real body (device and snapshot
// UUIDs included) must still fit the server's 256 KiB limit at any ID width.
func TestChunkBodiesFitServerLimitWithRealisticRows(t *testing.T) {
	for idLen := 40; idLen <= 80; idLen++ {
		var recs []Record
		for i := range 1200 {
			id := fmt.Sprintf("resp_%0*d", idLen-len("resp_"), i)
			r := testRecord(id)
			r.SessionID = fmt.Sprintf("019a%04x-5f8f-7c71-94cf-ec6ca289a888", i)
			r.ParentSessionID = "019a2024-5f8f-7c71-94cf-ec6ca289a888"
			r.Input, r.Output, r.CacheRead = int64(7361+i), int64(20+i), int64(12800+i)
			recs = append(recs, r)
		}
		var largest int
		f := newFakeContrib(t, func(w http.ResponseWriter, body []byte) {
			var req struct {
				Action     string `json:"action"`
				SnapshotID string `json:"snapshot_id"`
			}
			_ = json.Unmarshal(body, &req)
			if len(body) > maxChunkBytes {
				ack(w, http.StatusRequestEntityTooLarge, `{"error":"too_large"}`)
				return
			}
			switch req.Action {
			case "begin":
				ack(w, 200, `{"ok":true,"action":"begin","snapshot_id":"%s"}`, req.SnapshotID)
			case "chunk":
				largest = max(largest, len(body))
				ack(w, 200, `{"ok":true,"action":"chunk","snapshot_id":"%s"}`, req.SnapshotID)
			default:
				ack(w, 200, `{"ok":true,"action":"commit","snapshot_id":"%s","accepted":1200,"committed":true}`, req.SnapshotID)
			}
		})
		if accepted, err := Upload(context.Background(), f.url(), "k", newStateDir(t), "/r", uploadWindow, recs); err != nil || accepted != 1200 {
			t.Fatalf("id length %d: accepted=%d err=%v", idLen, accepted, err)
		}
		if largest <= maxChunkBytes-1024 {
			t.Fatalf("id length %d: largest chunk %d bytes never approached the limit; the test no longer exercises byte-bounded chunks", idLen, largest)
		}
	}
}

// TestUploadNoRedirects: a 3xx must surface as a protocol error and never be
// followed to another destination.
func TestUploadNoRedirects(t *testing.T) {
	f := newFakeContrib(t, func(w http.ResponseWriter, body []byte) {
		w.Header().Set("Location", "/elsewhere")
		w.WriteHeader(http.StatusFound)
	})
	_, err := Upload(context.Background(), f.url(), "k", newStateDir(t), "/r", uploadWindow, testRecords(60))
	if err == nil {
		t.Fatal("Upload silently accepted a redirect")
	}
	if f.other != 0 {
		t.Fatalf("redirect target hit %d times", f.other)
	}
}

// TestUploadRateLimitRetriesWithRetryAfter: a genuine rate-limit 429 carrying
// Retry-After is waited out and the identical request is retried.
func TestUploadRateLimitRetriesWithRetryAfter(t *testing.T) {
	attempts := 0
	f := newFakeContrib(t, func(w http.ResponseWriter, body []byte) {
		var req struct {
			Action     string `json:"action"`
			SnapshotID string `json:"snapshot_id"`
		}
		_ = json.Unmarshal(body, &req)
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "1")
			ack(w, 429, `{"error":"rate_limited","retry_after_seconds":1}`)
			return
		}
		switch req.Action {
		case "begin":
			ack(w, 200, `{"ok":true,"action":"begin","snapshot_id":"%s"}`, req.SnapshotID)
		case "chunk":
			ack(w, 200, `{"ok":true,"action":"chunk","snapshot_id":"%s"}`, req.SnapshotID)
		default:
			ack(w, 200, `{"ok":true,"action":"commit","snapshot_id":"%s","accepted":1200,"committed":true}`, req.SnapshotID)
		}
	})
	accepted, err := Upload(context.Background(), f.url(), "k", newStateDir(t), "/r", uploadWindow, testRecords(1200))
	if err != nil || accepted != 1200 {
		t.Fatalf("Upload: accepted=%d err=%v", accepted, err)
	}
	f.mu.Lock()
	begins := 0
	for _, body := range f.bodies {
		var req struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Action == "begin" {
			begins++
		}
	}
	f.mu.Unlock()
	if begins != 2 {
		t.Fatalf("begin requests = %d, want 2 (one retried)", begins)
	}
}

// TestUploadStagingLimitReturnsImmediately: a 429 without a valid Retry-After
// (staging limit) must abort immediately without retrying.
func TestUploadStagingLimitReturnsImmediately(t *testing.T) {
	f := newFakeContrib(t, func(w http.ResponseWriter, body []byte) {
		ack(w, 429, `{"error":"staging_limit"}`)
	})
	_, err := Upload(context.Background(), f.url(), "k", newStateDir(t), "/r", uploadWindow, testRecords(60))
	if !errors.Is(err, errRateLimited) {
		t.Fatalf("err = %v, want errRateLimited", err)
	}
	if got := f.requestCount(); got != 1 {
		t.Fatalf("staging limit retried: %d requests", got)
	}
}

// TestUploadEmptyRecordsNoMutationNoNetwork: the empty contract touches
// neither the network nor the state directory.
func TestUploadEmptyRecordsNoMutationNoNetwork(t *testing.T) {
	f := newFakeContrib(t, func(w http.ResponseWriter, body []byte) { t.Fatal("unexpected request") })
	stateDir := filepath.Join(t.TempDir(), "missing", "state")
	for _, recs := range [][]Record{nil, {}} {
		accepted, err := Upload(context.Background(), f.url(), "k", stateDir, "/r", uploadWindow, recs)
		if err != nil || accepted != 0 {
			t.Fatalf("empty Upload: accepted=%d err=%v", accepted, err)
		}
	}
	if f.requestCount() != 0 {
		t.Fatalf("network used for empty records: %d requests", f.requestCount())
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state dir was created for empty records")
	}
}
