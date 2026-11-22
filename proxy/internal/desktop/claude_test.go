package desktop

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

type claudeTestObject map[string]any

func claudeTestAttr(key, kind string, value any) claudeTestObject {
	return claudeTestObject{"key": key, "value": claudeTestObject{kind: value}}
}

func claudeTestEvent(id string, extra ...claudeTestObject) claudeTestObject {
	return claudeTestObject{
		"timeUnixNano": "1760000000123456789",
		"body":         claudeTestObject{"stringValue": "claude_code.api_request"},
		"attributes": append([]claudeTestObject{
			claudeTestAttr("request_id", "stringValue", id),
			claudeTestAttr("input_tokens", "intValue", "8"),
			claudeTestAttr("output_tokens", "intValue", 13),
			claudeTestAttr("cache_read_tokens", "intValue", 21),
			claudeTestAttr("cache_creation_tokens", "intValue", "34"),
		}, extra...),
	}
}

func claudeTestBatch(t *testing.T, resourceAttrs, scopeAttrs []claudeTestObject, events ...claudeTestObject) []byte {
	t.Helper()
	body, err := json.Marshal(claudeTestObject{"resourceLogs": []claudeTestObject{{
		"resource":  claudeTestObject{"attributes": resourceAttrs},
		"scopeLogs": []claudeTestObject{{"scope": claudeTestObject{"attributes": scopeAttrs}, "logRecords": events}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func claudeTestSend(h http.Handler, method, media, encoding string, body []byte, unknownLength bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/capability/v1/logs", bytes.NewReader(body))
	r.Header.Set("Content-Type", media)
	r.Header.Set("Content-Encoding", encoding)
	if unknownLength {
		r.ContentLength = -1
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestClaudeHandlerDesktopProjection(t *testing.T) {
	desktop := claudeTestAttr("app.entrypoint", "stringValue", "claude-desktop")
	cli := claudeTestAttr("app.entrypoint", "stringValue", "cli")
	private := claudeTestAttr("prompt", "stringValue", "private prompt text")
	const apiID = "req_011Native1"
	for _, tc := range []struct {
		name            string
		resource, scope []claudeTestObject
		event           claudeTestObject
		model           string
	}{
		{name: "desktop resource and body", resource: []claudeTestObject{desktop}, event: claudeTestEvent(apiID,
			claudeTestAttr("model", "stringValue", "claude-opus-5-5"), private,
			claudeTestAttr("session.id", "stringValue", "private session"),
			claudeTestAttr("cost_usd", "doubleValue", 1.25),
		), model: "claude-opus-5-5"},
		{name: "desktop event and attribute", event: func() claudeTestObject {
			event := claudeTestEvent(apiID, desktop, claudeTestAttr("event.name", "stringValue", "api_request"),
				claudeTestAttr("model", "stringValue", "private model text"))
			event["body"] = claudeTestObject{"stringValue": "private body text"}
			return event
		}(), model: "unknown"},
		{name: "desktop scope and event name", scope: []claudeTestObject{desktop}, event: func() claudeTestObject {
			event := claudeTestEvent(apiID)
			delete(event, "body")
			event["eventName"] = "claude_code.api_request"
			event["timeUnixNano"] = int64(1760000000123456789)
			return event
		}(), model: "unknown"},
		{name: "CLI event", event: claudeTestEvent(apiID, cli)},
		{name: "contradictory entrypoint", resource: []claudeTestObject{cli}, event: claudeTestEvent(apiID, desktop)},
		{name: "absent entrypoint", event: claudeTestEvent(apiID)},
		{name: "third-party provider", resource: []claudeTestObject{desktop, claudeTestAttr("gen_ai.provider.name", "stringValue", "aws.bedrock")}, event: claudeTestEvent(apiID)},
		{name: "different event", event: claudeTestEvent(apiID, desktop, claudeTestAttr("event.name", "stringValue", "api_error"))},
		{name: "non-provider ID", event: claudeTestEvent("req_untrusted private text", desktop)},
		{name: "invalid integer", event: func() claudeTestObject {
			event := claudeTestEvent(apiID, desktop)
			event["attributes"].([]claudeTestObject)[4] = claudeTestAttr("cache_creation_tokens", "intValue", "-1")
			return event
		}()},
		{name: "future wall time", event: func() claudeTestObject {
			event := claudeTestEvent(apiID, desktop)
			event["timeUnixNano"] = "18446744073709551615"
			return event
		}()},
		{name: "absent cache count", event: func() claudeTestObject {
			event := claudeTestEvent(apiID, desktop)
			attrs := event["attributes"].([]claudeTestObject)
			event["attributes"] = append(attrs[:4], attrs[5:]...)
			return event
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []Observation
			h := ClaudeHandler(func(o Observation) error { got = append(got, o); return nil })
			body := claudeTestBatch(t, tc.resource, tc.scope, tc.event)
			w := claudeTestSend(h, "POST", "application/json; charset=utf-8", "", body, false)
			if w.Code != http.StatusOK || w.Body.String() != "{}" {
				t.Fatalf("OTLP response: %d %q", w.Code, w.Body.String())
			}
			var want []Observation
			if tc.model != "" {
				want = []Observation{{
					Harness: Claude, CanonicalID: apiID, IdentityStrength: "request", Provider: "anthropic", Model: tc.model,
					Timestamp: time.Unix(1760000000, 123456789).UTC(), InputTokens: 8, OutputTokens: 13,
					CacheReadTokens: 21, CacheWriteTokens: 34,
				}}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("observations: got %+v, want %+v", got, want)
			}
		})
	}
}

func TestClaudeHandlerPersistenceRetry(t *testing.T) {
	var attempted []string
	persisted := make(map[string]Observation)
	fail := true
	h := ClaudeHandler(func(o Observation) error {
		attempted = append(attempted, o.CanonicalID)
		if fail && o.CanonicalID == "req_b" {
			return errors.New("private database error")
		}
		persisted[o.CanonicalID] = o
		return nil
	})
	body := claudeTestBatch(t, []claudeTestObject{claudeTestAttr("app.entrypoint", "stringValue", "claude-desktop")}, nil,
		claudeTestEvent("req_a"), nil, claudeTestEvent("req_b"), claudeTestEvent("req_c"))
	w := claudeTestSend(h, "POST", "application/json", "", body, false)
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "private") {
		t.Fatalf("retry response: %d %q", w.Code, w.Body.String())
	}
	if want := []string{"req_a", "req_b"}; !reflect.DeepEqual(attempted, want) || len(persisted) != 1 {
		t.Fatalf("first attempt: %v, persisted %v", attempted, persisted)
	}
	fail = false
	w = claudeTestSend(h, "POST", "application/json", "", body, false)
	if want := []string{"req_a", "req_b", "req_a", "req_b", "req_c"}; w.Code != http.StatusOK || !reflect.DeepEqual(attempted, want) || len(persisted) != 3 {
		t.Fatalf("replay: status %d, attempts %v, persisted %v", w.Code, attempted, persisted)
	}
}

func claudeTestGzip(t *testing.T, body []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	if _, err := z.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestClaudeHandlerTransport(t *testing.T) {
	valid := claudeTestBatch(t, []claudeTestObject{claudeTestAttr("app.entrypoint", "stringValue", "claude-desktop")}, nil, claudeTestEvent("req_native"))
	zip := claudeTestGzip(t, valid)
	// Trailing JSON whitespace is valid, so rejection just over the decoded cap must be the body limit.
	oversized := claudeTestGzip(t, append(bytes.Clone(valid), bytes.Repeat([]byte(" "), claudeJSONLimit+1-len(valid))...))
	for _, tc := range []struct {
		name, method, media, encoding string
		body                          []byte
		unknownLength                 bool
		status, writes                int
	}{
		{name: "valid gzip", method: "POST", media: "application/json", encoding: "gzip", body: zip, status: 200, writes: 1},
		{name: "truncated gzip", method: "POST", media: "application/json", encoding: "gzip", body: zip[:len(zip)-1], status: 400},
		{name: "decoded cap", method: "POST", media: "application/json", encoding: "gzip", body: oversized, status: 413},
		{name: "wire cap", method: "POST", media: "application/json", body: make([]byte, claudeWireLimit+1), unknownLength: true, status: 413},
		{name: "unsupported encoding", method: "POST", media: "application/json", encoding: "br", body: valid, status: 415},
		{name: "unsupported media", method: "POST", media: "application/x-protobuf", body: valid, status: 415},
		{name: "wrong method", method: "PUT", media: "application/json", body: valid, status: 405},
		{name: "broken envelope", method: "POST", media: "application/json", body: []byte(`{"resourceLogs":true}`), status: 400},
		{name: "trailing document", method: "POST", media: "application/json", body: append(bytes.Clone(valid), []byte(`{}`)...), status: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			h := ClaudeHandler(func(Observation) error { writes++; return nil })
			w := claudeTestSend(h, tc.method, tc.media, tc.encoding, tc.body, tc.unknownLength)
			if w.Code != tc.status || writes != tc.writes || w.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("response %d %q; writes %d", w.Code, w.Body.String(), writes)
			}
		})
	}
}
