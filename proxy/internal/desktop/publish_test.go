package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDesktopEndpointValidation(t *testing.T) {
	for input, want := range map[string]string{
		"https://MY-PROJECT.supabase.co/functions/v1/ingest": "https://my-project.supabase.co/functions/v1/contributions",
		"http://127.0.0.1:1234/ingest":                       "http://127.0.0.1:1234/contributions",
		"http://[::1]:1234/ingest":                           "http://[::1]:1234/contributions",
	} {
		if got, err := contributionEndpoint(input); err != nil || got != want {
			t.Errorf("%s => %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{
		"", "ingest", "//example.org/ingest", "ftp://localhost/ingest", "http://example.org/ingest",
		"http://localhost/ingest", "http://localhost.example.org/ingest", "http://127.0.0.1.example.org/ingest", "https://a:b:c/ingest",
		"https://secret@example.org/ingest", "https://example.org/ingest#", "https://example.org/ingest?",
		"https://example.org/ingest/", "https://example.org/other", "https://example.org//ingest",
		"https://example.org/../ingest", "https://example.org/%2e/ingest", "https://example.org:65536/ingest",
	} {
		if err := ValidateEndpoint(input); !errors.Is(err, errEndpoint) {
			t.Errorf("accepted invalid endpoint %s", input)
		}
	}
}

func TestDesktopSnapshotReplayAndIncrementalQueue(t *testing.T) {
	opts := ledgerOptions(t)
	var mu sync.Mutex
	var bodies []string
	var firstCollector *Collector
	var loseAck = true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/contributions" || r.Header.Get("Authorization") != "Bearer "+opts.APIKey || r.Method != "POST" {
			t.Error("invalid request routing or authentication")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(r.Body)
		var req publication
		if err != nil || json.Unmarshal(data, &req) != nil {
			t.Error("invalid request")
			w.WriteHeader(400)
			return
		}
		bodies = append(bodies, string(data))
		if req.Action == "begin" && loseAck {
			// Capture independently during delivery; it must not change the staged snapshot.
			if err := firstCollector.Record(ledgerObservation("concurrent", ledgerEpoch)); err != nil {
				t.Error(err)
			}
		}
		if req.Action == "commit" && loseAck {
			loseAck = false
			// A successful remote commit whose acknowledgment did not arrive intact.
			fmt.Fprint(w, `{"ok":`)
			return
		}
		accepted := 1
		if len(bodies) == 6 {
			accepted = 100
		}
		fmt.Fprintf(w, `{"ok":true,"action":%q,"snapshot_id":%q,"committed":true,"accepted":%d}`, req.Action, req.Snapshot, accepted)
	}))
	defer server.Close()
	opts.IngestURL = server.URL + "/ingest"
	c := openLedger(t, opts)
	mu.Lock()
	firstCollector = c
	mu.Unlock()
	// Pausing still allows delivery of work already captured. Tests bypass the
	// timer and exercise a single bounded publication step deterministically.
	for i := range 100 {
		ledgerRecord(t, c, fmt.Sprintf("id%d", i), ledgerEpoch.Add(5*time.Millisecond))
	}
	if _, err := c.publish(context.Background(), Claude); !errors.Is(err, errDelivery) {
		t.Fatalf("lost acknowledgment returned %v", err)
	}
	if c.Pending() != 101 {
		t.Fatalf("lost acknowledgment changed pending: %d", c.Pending())
	}
	if err := c.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = openLedger(t, opts)
	if _, err := c.publish(context.Background(), Claude); err != nil {
		t.Fatal(err)
	}
	if c.Pending() != 1 {
		t.Fatalf("first batch left %d records", c.Pending())
	}
	if _, err := c.publish(context.Background(), Claude); err != nil {
		t.Fatal(err)
	}
	if c.Pending() != 0 {
		t.Fatalf("second batch left %d records", c.Pending())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 9 {
		t.Fatalf("sent %d requests", len(bodies))
	}
	for i, before := range bodies[:3] {
		if bodies[i+3] != before {
			t.Fatalf("snapshot replay changed body index %d", i)
		}
	}
	var first, second struct {
		Observations []wireObservation `json:"observations"`
	}
	if err := json.Unmarshal([]byte(bodies[1]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(bodies[7]), &second); err != nil {
		t.Fatal(err)
	}
	if len(first.Observations) != 100 || len(second.Observations) != 1 || second.Observations[0].CanonicalID != "req_concurrent" {
		t.Fatal("batches did not contain only selected observations")
	}
	row := first.Observations[0]
	if row.Timestamp != "2025-05-10T12:00:00.005Z" || row.CanonicalID != "req_id0" || row.Model != "unknown" || row.Grain != "request" || row.CallCount != 1 || row.CostUSD != nil || row.CostBasis != "unknown" || row.SessionID != nil || row.ParentSessionID != nil || row.IsSubagent != nil || row.InputTokens != 30 || row.OutputTokens != 20 || row.CacheReadTokens != 10 || row.CacheWriteTokens != 5 {
		t.Fatal("wire observation did not match request accounting contract")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = openLedger(t, opts)
	if err := c.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	ledgerRecord(t, c, "id0", ledgerEpoch.Add(5*time.Millisecond))
	if c.Pending() != 0 {
		t.Fatal("acknowledged identity was not deduplicated across restart")
	}
}

func TestDesktopPublisherRedirectionRetryAndConflict(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Store(true) }))
	defer target.Close()
	var mode atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 0:
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		case 1:
			w.Header().Set("Retry-After", "17")
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":"ownership_conflict","canonical_id":"secret-app-content"}`)
		}
	}))
	defer server.Close()
	opts := ledgerOptions(t)
	opts.IngestURL = server.URL + "/ingest"
	c := openLedger(t, opts)
	ledgerRecord(t, c, "initial", ledgerEpoch)
	if _, err := c.publish(context.Background(), Claude); err == nil || redirected.Load() {
		t.Fatal("redirect was followed or accepted")
	}
	pending := c.sources[Claude].pending.ID
	mode.Store(1)
	if retry, err := c.publish(context.Background(), Claude); err == nil || retry != 17*time.Second {
		t.Fatalf("rate limit: retry %s, error %v", retry, err)
	}
	if got := retryAfter(ledgerEpoch.Add(41*time.Second).Format(http.TimeFormat), ledgerEpoch); got != 41*time.Second {
		t.Fatalf("HTTP-date retry %s", got)
	}
	mode.Store(2)
	if _, err := c.publish(context.Background(), Claude); !errors.Is(err, errConflict) || strings.Contains(err.Error(), "secret-app-content") {
		t.Fatalf("conflict returned %v", err)
	}
	if c.Pending() != 1 || c.sources[Claude].pending.ID != pending {
		t.Fatal("conflict discarded or replaced the pending snapshot")
	}
}
