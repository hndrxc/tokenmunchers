package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "tm_" + "yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy"

func testEvent(now time.Time, session string) UsageEvent {
	return UsageEvent{
		Kind: "usage", EventID: NewUUIDv4(), SessionID: session,
		Timestamp: now.Format("2006-01-02T15:04:05.000Z"),
		Provider:  "anthropic", Model: "claude-sonnet-4-6",
		InputTokens: 10, OutputTokens: 5,
	}
}

func TestValidateIngestURLPolicy(t *testing.T) {
	good := []string{
		"https://example.com/functions/v1/ingest",
		"https://example.com:8443/functions/v1/ingest",
		"http://127.0.0.1:8080/functions/v1/ingest",
		"http://[::1]:8080/functions/v1/ingest",
		"http://127.1.2.3/ingest",
	}
	for _, raw := range good {
		if _, err := validateIngestURL(raw); err != nil {
			t.Errorf("expected %q accepted: %v", raw, err)
		}
	}
	bad := []string{
		"",
		"example.com/ingest",
		"/functions/v1/ingest",
		"//example.com/ingest",
		"https:///ingest",
		"https://",
		"ftp://example.com/ingest",
		"file:///etc/passwd",
		"http://example.com/ingest",
		"http://localhost:8080/ingest",
		"http://0.0.0.0/ingest",
		"http://10.0.0.1/ingest",
		"http://127.0.0.1.example.com/ingest",
		"https://user@example.com/ingest",
		"https://user:pass@example.com/ingest",
		"http://user:pass@127.0.0.1/ingest",
		"https://example.com/ingest?token=1",
		"https://example.com/ingest?",
		"https://example.com/ingest#frag",
		"https://example.com/ingest#",
		"https:example.com/ingest",
		"https://exa mple.com/ingest",
	}
	for _, raw := range bad {
		if _, err := validateIngestURL(raw); err == nil {
			t.Errorf("expected %q rejected", raw)
		} else if strings.Contains(err.Error(), "example.com") || strings.Contains(err.Error(), "user") {
			t.Errorf("error for %q leaks input: %v", raw, err)
		}
	}
}

func TestInvalidDestinationBlocksNetworkAndKeepsQueue(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	now := time.Now().UTC()
	// Valid loopback host but with credentials/query: must be refused before any I/O.
	for _, dest := range []string{
		strings.Replace(srv.URL, "http://", "http://user:secret@", 1),
		srv.URL + "/ingest?k=secret",
		srv.URL + "/ingest#secret",
	} {
		queuePath := filepath.Join(t.TempDir(), "queue.jsonl")
		rep := NewReporter(Options{QueuePath: queuePath, IngestURL: dest, APIKey: testKey, Now: func() time.Time { return now }})
		if err := rep.Enqueue([]UsageEvent{testEvent(now, "s")}); err != nil {
			t.Fatal(err)
		}
		res, err := rep.Send(context.Background(), []UsageEvent{testEvent(now, "s")})
		if res != SendRejected || err == nil {
			t.Fatalf("Send must reject invalid destination, got %v %v", res, err)
		}
		if err := rep.Flush(context.Background(), true); err == nil {
			t.Fatal("Flush must fail on invalid destination")
		}
		if rep.QueueSize() != 1 {
			t.Fatalf("queue must be preserved, got %d", rep.QueueSize())
		}
		for _, msg := range []string{err.Error(), rep.Stats().LastError} {
			if strings.Contains(msg, "secret") || strings.Contains(msg, srv.URL) || strings.Contains(msg, testKey) {
				t.Fatalf("message leaks destination or key: %q", msg)
			}
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("no request may reach the server, got %d", hits.Load())
	}
}

func TestRedirectsAreNeverFollowedAndCallerClientUntouched(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	injected := &http.Client{Timeout: 5 * time.Second}
	now := time.Now().UTC()
	rep := NewReporter(Options{IngestURL: redirector.URL, APIKey: testKey, HTTPClient: injected, Now: func() time.Time { return now }})

	res, err := rep.Send(context.Background(), []UsageEvent{testEvent(now, "s")})
	if res != SendRetry || err == nil {
		t.Fatalf("expected retry on redirect response, got %v %v", res, err)
	}
	if !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("expected status-only error, got %q", err)
	}
	if strings.Contains(err.Error(), target.URL) || strings.Contains(rep.Stats().LastError, target.URL) {
		t.Fatal("redirect target leaked into error")
	}
	if targetHits.Load() != 0 {
		t.Fatal("redirect was followed")
	}
	if injected.CheckRedirect != nil {
		t.Fatal("caller's HTTP client was mutated")
	}
}

func TestErrorsExposeStatusOnly(t *testing.T) {
	const leak = "SECRET-REMOTE-TEXT /home/victim/.config token=abc"
	var mu sync.Mutex
	status := http.StatusInternalServerError
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		code := status
		mu.Unlock()
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": leak, "detail": leak})
	}))
	defer srv.Close()

	now := time.Now().UTC()
	queuePath := filepath.Join(t.TempDir(), "queue.jsonl")
	rep := NewReporter(Options{QueuePath: queuePath, IngestURL: srv.URL, APIKey: testKey, Now: func() time.Time { return now }})

	for _, code := range []int{http.StatusInternalServerError, http.StatusUnauthorized, http.StatusBadRequest} {
		mu.Lock()
		status = code
		mu.Unlock()
		_, err := rep.Send(context.Background(), []UsageEvent{testEvent(now, "s")})
		if err == nil {
			t.Fatalf("status %d: expected error", code)
		}
		want := statusError(code).Error()
		if err.Error() != want || rep.Stats().LastError != want {
			t.Fatalf("status %d: got err=%q last=%q want %q", code, err, rep.Stats().LastError, want)
		}
	}

	// Flush path surfaces the same sanitized text.
	mu.Lock()
	status = http.StatusServiceUnavailable
	mu.Unlock()
	if err := rep.Enqueue([]UsageEvent{testEvent(now, "s")}); err != nil {
		t.Fatal(err)
	}
	err := rep.Flush(context.Background(), true)
	if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "/home") {
		t.Fatalf("unsanitized flush error: %v", err)
	}
	if st := rep.Stats(); strings.Contains(st.LastError, "SECRET") || !strings.Contains(st.LastError, "HTTP 503") {
		t.Fatalf("unsanitized stats error: %q", st.LastError)
	}
}

func TestTransportErrorDoesNotLeakURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // connection refused

	now := time.Now().UTC()
	rep := NewReporter(Options{IngestURL: url + "/functions/v1/ingest", APIKey: testKey, Now: func() time.Time { return now }})
	res, err := rep.Send(context.Background(), []UsageEvent{testEvent(now, "s")})
	if res != SendRetry || err == nil {
		t.Fatalf("expected retry on transport failure, got %v %v", res, err)
	}
	for _, msg := range []string{err.Error(), rep.Stats().LastError} {
		if strings.Contains(msg, "127.0.0.1") || strings.Contains(msg, "functions") || strings.Contains(msg, "refused") {
			t.Fatalf("transport error leaks detail: %q", msg)
		}
	}
}

func TestEnqueueErrorDoesNotLeakPath(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	queuePath := filepath.Join(blocker, "sub", "queue.jsonl")
	rep := NewReporter(Options{QueuePath: queuePath, IngestURL: "https://example.com/ingest", APIKey: testKey})
	err := rep.Enqueue([]UsageEvent{testEvent(time.Now().UTC(), "s")})
	if err == nil || strings.Contains(err.Error(), blocker) || strings.Contains(err.Error(), "/") {
		t.Fatalf("expected sanitized enqueue error, got %v", err)
	}
}

func TestLegacyQueueEntriesAreRedactedAndMarkerNeverOnWire(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		b, _ := json.Marshal(raw)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	now := time.Now().UTC()
	queuePath := filepath.Join(t.TempDir(), "queue.jsonl")

	// Legacy entry (no marker) whose session ID imitates the safe prefix.
	legacy := testEvent(now, "tmproxy:v2:raw-header-value")
	legacyLine, _ := json.Marshal(legacy)
	if err := os.WriteFile(queuePath, append(legacyLine, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	rep := NewReporter(Options{QueuePath: queuePath, IngestURL: srv.URL, APIKey: testKey, Now: func() time.Time { return now }})
	if err := rep.Enqueue([]UsageEvent{testEvent(now, "safe-opaque-id")}); err != nil {
		t.Fatal(err)
	}
	if err := rep.Flush(context.Background(), true); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	all := strings.Join(bodies, "\n")
	mu.Unlock()
	if strings.Contains(all, "raw-header-value") {
		t.Fatalf("legacy session data was uploaded: %s", all)
	}
	if !strings.Contains(all, LegacySessionID) || !strings.Contains(all, "safe-opaque-id") {
		t.Fatalf("unexpected wire bodies: %s", all)
	}
	if strings.Contains(all, "tm_queue") {
		t.Fatalf("queue marker leaked onto the wire: %s", all)
	}
}

func TestRequeuePreservesSafeMarker(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var raw map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		b, _ := json.Marshal(raw)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	now := time.Now().UTC()
	queuePath := filepath.Join(t.TempDir(), "queue.jsonl")
	rep := NewReporter(Options{QueuePath: queuePath, IngestURL: srv.URL, APIKey: testKey, Now: func() time.Time { return now }})
	if err := rep.Enqueue([]UsageEvent{testEvent(now, "safe-opaque-id")}); err != nil {
		t.Fatal(err)
	}
	if err := rep.Flush(context.Background(), true); err == nil {
		t.Fatal("expected failure")
	}
	fail.Store(false)
	if err := rep.Flush(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	all := strings.Join(bodies, "\n")
	mu.Unlock()
	if !strings.Contains(all, "safe-opaque-id") || strings.Contains(all, LegacySessionID) {
		t.Fatalf("requeued safe event was wrongly redacted: %s", all)
	}
}

// blockQueueAppends makes every Enqueue fail deterministically (no chmod/root
// dependence): a directory occupying the queue path cannot be opened for append.
func blockQueueAppends(t *testing.T, queuePath string) {
	t.Helper()
	if err := os.Mkdir(queuePath, 0o700); err != nil {
		t.Fatal(err)
	}
}

func claimedFiles(t *testing.T, queuePath string) []string {
	t.Helper()
	files, err := filepath.Glob(queuePath + ".*.flushing")
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestFailedRequeueKeepsClaimedFileAndRetriesUnderBackoff(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	var mu sync.Mutex
	var bodies []string
	var queuePath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			// The queue becomes unwritable while delivery is in flight.
			if st, err := os.Stat(queuePath); err != nil || !st.IsDir() {
				_ = os.Mkdir(queuePath, 0o700)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var raw map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		b, _ := json.Marshal(raw)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	now := time.Now().UTC()
	dir := t.TempDir()
	queuePath = filepath.Join(dir, "queue.jsonl")
	rep := NewReporter(Options{QueuePath: queuePath, IngestURL: srv.URL, APIKey: testKey, Now: func() time.Time { return now }})
	a, b := testEvent(now, "safe-a"), testEvent(now, "safe-b")
	if err := rep.Enqueue([]UsageEvent{a, b}); err != nil {
		t.Fatal(err)
	}

	err := rep.Flush(context.Background(), false)
	if err == nil {
		t.Fatal("expected failure")
	}
	files := claimedFiles(t, queuePath)
	if len(files) != 1 {
		t.Fatalf("claimed file must be retained, got %v", files)
	}
	kept, rerr := os.ReadFile(files[0])
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, ev := range []UsageEvent{a, b} {
		if !strings.Contains(string(kept), ev.EventID) {
			t.Fatalf("retained file lost event %s", ev.EventID)
		}
	}
	st := rep.Stats()
	if st.Retained != 1 || !strings.Contains(st.LastError, "HTTP 503") || !strings.Contains(st.LastError, "kept for retry") {
		t.Fatalf("diagnostics: %+v", st)
	}
	if strings.Contains(st.LastError, dir) || strings.Contains(err.Error(), dir) {
		t.Fatalf("diagnostics leak path: %q / %q", st.LastError, err)
	}

	// Recover: queue writable again, server healthy.
	if err := os.Remove(queuePath); err != nil {
		t.Fatal(err)
	}
	fail.Store(false)

	// Still inside backoff: nothing is sent, nothing is lost.
	if err := rep.Flush(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	n := len(bodies)
	mu.Unlock()
	if n != 0 || len(claimedFiles(t, queuePath)) != 1 {
		t.Fatalf("retry ran inside backoff window (bodies=%d)", n)
	}

	// After backoff (well inside the 5-minute stale window) the retained
	// claim is retried.
	now = now.Add(BaseBackoff + time.Second)
	if err := rep.Flush(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	all := strings.Join(bodies, "\n")
	mu.Unlock()
	if !strings.Contains(all, a.EventID) || !strings.Contains(all, b.EventID) {
		t.Fatalf("retained events not delivered: %s", all)
	}
	if left := claimedFiles(t, queuePath); len(left) != 0 {
		t.Fatalf("claimed file should be removed after delivery: %v", left)
	}
	if st := rep.Stats(); st.Retained != 0 || st.LastError != "" {
		t.Fatalf("stats after recovery: %+v", st)
	}
}

func TestPartialDeliveryThenFailedRequeueShrinksRetainedFile(t *testing.T) {
	var calls atomic.Int32
	var queuePath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = os.Mkdir(queuePath, 0o700)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	now := time.Now().UTC()
	queuePath = filepath.Join(t.TempDir(), "queue.jsonl")
	rep := NewReporter(Options{QueuePath: queuePath, IngestURL: srv.URL, APIKey: testKey, Now: func() time.Time { return now }})
	var events []UsageEvent
	for i := 0; i < MaxBatchEvents+1; i++ {
		events = append(events, testEvent(now, "safe"))
	}
	if err := rep.Enqueue(events); err != nil {
		t.Fatal(err)
	}
	if err := rep.Flush(context.Background(), false); err == nil {
		t.Fatal("expected failure")
	}
	files := claimedFiles(t, queuePath)
	if len(files) != 1 {
		t.Fatalf("claimed file must be retained, got %v", files)
	}
	kept, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if lines := strings.Count(string(kept), "\n"); lines != 1 || !strings.Contains(string(kept), last.EventID) {
		t.Fatalf("retained file should hold only the undelivered event, got %d lines", lines)
	}
	if !strings.Contains(string(kept), `"tm_queue":2`) {
		t.Fatalf("retained entry lost its content: %s", kept)
	}
}
