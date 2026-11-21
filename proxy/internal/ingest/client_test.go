package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func TestUUIDGenerators(t *testing.T) {
	v4 := NewUUIDv4()
	if !uuidRE.MatchString(v4) {
		t.Fatalf("invalid UUIDv4: %q", v4)
	}
	v5a := NewUUIDv5("anthropic|msg_1|claude-sonnet-4-6|0")
	v5b := NewUUIDv5("anthropic|msg_1|claude-sonnet-4-6|0")
	if !uuidRE.MatchString(v5a) || v5a != v5b {
		t.Fatalf("UUIDv5 not deterministic/valid: %q vs %q", v5a, v5b)
	}
}

func TestQueueRetryAndSingleBadEventIsolation(t *testing.T) {
	var attempts atomic.Int32
	var received []UsageEvent

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tm_"+strings.Repeat("x", 43) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		n := attempts.Add(1)
		if n == 1 {
			// Simulate transient outage
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"storage_failed"}`))
			return
		}

		var raw map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var batch []UsageEvent
		if evs, ok := raw["events"]; ok {
			_ = json.Unmarshal(evs, &batch)
		} else {
			b, _ := json.Marshal(raw)
			var one UsageEvent
			_ = json.Unmarshal(b, &one)
			batch = []UsageEvent{one}
		}

		// Simulate server rejecting index 0 on second attempt because model == "bad-model"
		for i, ev := range batch {
			if ev.Model == "bad-model" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error":  "invalid_event",
					"index":  i,
					"detail": "invalid model",
				})
				return
			}
		}

		received = append(received, batch...)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]int{"accepted": len(batch), "inserted": len(batch)})
	}))
	defer srv.Close()

	queuePath := filepath.Join(t.TempDir(), "queue.jsonl")
	now := time.Now().UTC()
	rep := NewReporter(Options{
		QueuePath: queuePath,
		IngestURL: srv.URL,
		APIKey:    "tm_" + strings.Repeat("x", 43),
		Now:       func() time.Time { return now },
	})

	ev1 := UsageEvent{
		Kind: "usage", EventID: NewUUIDv4(), SessionID: "s1",
		Timestamp: now.Format("2006-01-02T15:04:05.000Z"),
		Provider: "anthropic", Model: "bad-model",
		InputTokens: 10, OutputTokens: 20,
	}
	ev2 := UsageEvent{
		Kind: "usage", EventID: NewUUIDv4(), SessionID: "s1",
		Timestamp: now.Format("2006-01-02T15:04:05.000Z"),
		Provider: "anthropic", Model: "claude-sonnet-4-6",
		InputTokens: 100, OutputTokens: 50,
	}
	if err := rep.Enqueue([]UsageEvent{ev1, ev2}); err != nil {
		t.Fatal(err)
	}

	// Attempt 1 fails with 503 -> both remain queued.
	if err := rep.Flush(context.Background(), true); err == nil {
		t.Fatal("expected error on 503")
	}
	if rep.QueueSize() != 2 {
		t.Fatalf("expected 2 events still queued after 503, got %d", rep.QueueSize())
	}

	// Attempt 2 drops bad-model on 400 index=0 and delivers ev2!
	if err := rep.Flush(context.Background(), true); err != nil {
		t.Fatalf("expected second flush to succeed, got %v", err)
	}
	if rep.QueueSize() != 0 {
		t.Fatalf("expected empty queue after flush, got %d", rep.QueueSize())
	}
	if len(received) != 1 || received[0].Model != "claude-sonnet-4-6" {
		t.Fatalf("expected only good event delivered, got %+v", received)
	}
	st := rep.Stats()
	if st.Delivered != 1 || st.Dropped != 1 {
		t.Fatalf("unexpected stats: %+v", st)
	}
}
