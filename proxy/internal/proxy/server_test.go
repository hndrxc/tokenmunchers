package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/config"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/ingest"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/localstore"
	"github.com/tokenmunchers/tokenmunchers/proxy/internal/pricing"
)

// strictAllowedFields mirrors supabase/functions/ingest/index.ts exactly.
var strictAllowedFields = map[string]bool{
	"kind":               true,
	"event_id":           true,
	"session_id":         true,
	"ts":                 true,
	"provider":           true,
	"model":              true,
	"input_tokens":       true,
	"output_tokens":      true,
	"cache_read_tokens":  true,
	"cache_write_tokens": true,
	"cost_usd":           true,
	"is_subagent":        true,
	"client_version":     true,
}

func TestEndToEndProxyToIngest(t *testing.T) {
	// 1. Mock Anthropic upstream (checks Authorization passthrough and streams SSE)
	var seenAuth string
	var seenBodyLen int
	anthropicUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("x-api-key")
		b, _ := io.ReadAll(r.Body)
		seenBodyLen = len(b)
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("request-id", "req_upstream_1")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_e2e\",\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":200,\"output_tokens\":1,\"cache_read_input_tokens\":50,\"cache_creation_input_tokens\":10}}}\n\n"))
		if fl != nil {
			fl.Flush()
		}
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":75}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer anthropicUp.Close()

	// 2. Mock TokenMunchers /functions/v1/ingest validating strict allowlist
	var mu sync.Mutex
	var capturedPayloads []map[string]any
	apiKey := "tm_" + strings.Repeat("Z", 43)

	ingestSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+apiKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var raw map[string]any
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var items []map[string]any
		if evs, ok := raw["events"].([]any); ok {
			for _, it := range evs {
				items = append(items, it.(map[string]any))
			}
		} else {
			items = []map[string]any{raw}
		}
		for _, item := range items {
			for k := range item {
				if !strictAllowedFields[k] {
					t.Errorf("forbidden field sent to ingest: %q", k)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
		}
		mu.Lock()
		capturedPayloads = append(capturedPayloads, items...)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"accepted":1,"inserted":1}`))
	}))
	defer ingestSrv.Close()

	// 3. Spin up proxy Server
	tmp := t.TempDir()
	paths := config.Paths{ConfigDir: filepath.Join(tmp, "cfg"), StateDir: filepath.Join(tmp, "state")}
	cfg := config.Config{
		ListenAddr: "127.0.0.1:0",
		IngestURL:  ingestSrv.URL,
		APIKey:     apiKey,
		Routes: map[string]config.Route{
			"anthropic": {
				Name:     "anthropic",
				Upstream: anthropicUp.URL,
				Provider: "anthropic",
				Format:   config.FormatAnthropic,
			},
		},
	}
	rep := ingest.NewReporter(ingest.Options{
		QueuePath: paths.QueueFile(),
		IngestURL: ingestSrv.URL,
		APIKey:    apiKey,
	})
	rep.Start()
	defer rep.Stop(context.Background())

	store := localstore.New(paths.UsageLogFile())
	srv, err := New(Options{
		Config:     cfg,
		Paths:      paths,
		Pricing:    pricing.NewTable(nil),
		Reporter:   rep,
		LocalStore: store,
		LocalKey:   testKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	proxyHTTP := httptest.NewServer(srv)
	defer proxyHTTP.Close()

	// 4. Send a large (> MaxRequestModelPeek) POST request with secret header & session header.
	largePrompt := strings.Repeat("A", MaxRequestModelPeek+4096)
	reqBody := []byte(`{"model":"claude-sonnet-4-6","prompt":"` + largePrompt + `"}`)
	req, err := http.NewRequest(http.MethodPost, proxyHTTP.URL+testKey.RoutePrefix()+"/anthropic/v1/messages", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-ant-super-secret")
	req.Header.Set("x-claude-code-session-id", "claude-sess-42")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", resp.StatusCode, string(respBytes))
	}
	if got := resp.Header.Get("Content-Security-Policy"); !strings.Contains(got, "sandbox") || !strings.Contains(got, "default-src 'none'") {
		t.Fatalf("expected strict CSP with sandbox, got %q", got)
	}
	if seenAuth != "sk-ant-super-secret" {
		t.Fatalf("expected x-api-key passed through unchanged, got %q", seenAuth)
	}
	if seenBodyLen != len(reqBody) {
		t.Fatalf("large request body truncated: got %d, want %d", seenBodyLen, len(reqBody))
	}

	// Wait briefly for async flush to deliver to mock ingest.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(capturedPayloads)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(capturedPayloads) != 1 {
		t.Fatalf("expected 1 ingest event delivered, got %d", len(capturedPayloads))
	}
	ev := capturedPayloads[0]
	session, _ := ev["session_id"].(string)
	if ev["provider"] != "anthropic" || ev["model"] != "claude-sonnet-4-6" ||
		!strings.HasPrefix(session, "tmproxy:v2:") || strings.Contains(session, "claude-sess-42") {
		t.Fatalf("unexpected ingest metadata: %+v", ev)
	}
	if ev["input_tokens"].(float64) != 200 || ev["output_tokens"].(float64) != 75 || ev["cache_read_tokens"].(float64) != 50 || ev["cache_write_tokens"].(float64) != 10 {
		t.Fatalf("unexpected ingest token counts: %+v", ev)
	}
	if ev["cost_usd"].(float64) <= 0 {
		t.Fatalf("expected estimated cost_usd > 0, got %v", ev["cost_usd"])
	}

	// 5. Verify pause stops recording while still proxying traffic!
	pauseReq, _ := http.NewRequest(http.MethodPost, proxyHTTP.URL+"/_tmproxy/pause", nil)
	pauseReq.Header.Set(controlHeader, testKey.ControlToken())
	pauseResp, err := http.DefaultClient.Do(pauseReq)
	if err != nil || pauseResp.StatusCode != 200 {
		t.Fatalf("pause failed: %v %v", err, pauseResp)
	}
	pauseResp.Body.Close()

	req2, _ := http.NewRequest(http.MethodPost, proxyHTTP.URL+testKey.RoutePrefix()+"/anthropic/v1/messages", bytes.NewReader([]byte(`{"model":"claude-sonnet-4-6"}`)))
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil || resp2.StatusCode != 200 {
		t.Fatalf("expected paused request to still succeed, got %v", err)
	}
	_, _ = io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if srv.Status().SkippedPause != 1 {
		t.Fatalf("expected skipped_paused=1, got %d", srv.Status().SkippedPause)
	}

	// 6. Verify security guards (Origin header & WebSocket upgrade).
	evilReq, _ := http.NewRequest(http.MethodGet, proxyHTTP.URL+"/_tmproxy/status", nil)
	evilReq.Header.Set(controlHeader, testKey.ControlToken())
	evilReq.Header.Set("Origin", "https://evil.example")
	evilResp, err := http.DefaultClient.Do(evilReq)
	if err != nil || evilResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for Origin header, got %v", evilResp)
	}
	evilResp.Body.Close()

	wsReq, _ := http.NewRequest(http.MethodGet, proxyHTTP.URL+testKey.RoutePrefix()+"/anthropic/v1/messages", nil)
	wsReq.Header.Set("Upgrade", "websocket")
	wsResp, err := http.DefaultClient.Do(wsReq)
	if err != nil || wsResp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("expected 426 for websocket upgrade, got %v", wsResp)
	}
	wsResp.Body.Close()
}

func TestLocalBoundary(t *testing.T) {
	if _, err := New(Options{LocalKey: testKey, Config: config.Config{ListenAddr: "0.0.0.0:8787"}}); err == nil {
		t.Fatal("accepted a wildcard listener")
	}
	srv, err := New(Options{LocalKey: testKey, Config: config.Config{
		APIKey:    "tm_sensitive_value",
		IngestURL: "https://ingest.example/private-path",
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, host, header string
	}{
		{"missing Host", "", ""},
		{"foreign Host", "example.test", ""},
		{"empty Origin", "localhost", "Origin"},
		{"browser fetch", "localhost", "Sec-Fetch-Site"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/_tmproxy/status", nil)
			req.Host = tc.host
			if tc.header != "" {
				req.Header.Set(tc.header, "")
			}
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
		})
	}
	rec := httptest.NewRecorder()
	statusReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/_tmproxy/status", nil)
	statusReq.Header.Set(controlHeader, testKey.ControlToken())
	srv.ServeHTTP(rec, statusReq)
	var status StatusSnapshot
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &status) != nil ||
		status.IngestURL != "https://ingest.example" || status.APIKeyMasked != "configured" {
		t.Fatalf("unexpected status response: %d %s", rec.Code, rec.Body)
	}
}

func TestSessionPseudonyms(t *testing.T) {
	a, err := New(Options{LocalKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(Options{LocalKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	req.Header.Set("User-Agent", "client-private-value")
	if got := a.deriveSessionID(req, "openai"); got != "tmproxy:v2:route:openai" {
		t.Fatalf("unexpected fallback session metadata: %q", got)
	}
	req.Header.Set("X-Session-Id", "private-value")
	id := a.deriveSessionID(req, "openai")
	if id == "private-value" || id != a.deriveSessionID(req, "openai") || id == b.deriveSessionID(req, "openai") {
		t.Fatal("session IDs must be pseudonymous, stable per instance, and different across instances")
	}
}
