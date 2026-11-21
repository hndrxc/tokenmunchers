package desktop

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	claudeWireLimit = 2 << 20
	claudeJSONLimit = 8 << 20
)

// ClaudeHandler receives OTLP/HTTP JSON logs on the private Desktop capability route.
// The caller is responsible for restricting access to that route.
func ClaudeHandler(record func(Observation) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			claudeOTLPReply(w, http.StatusMethodNotAllowed, `{"code":12,"message":"POST required"}`)
			return
		}
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			claudeOTLPReply(w, http.StatusUnsupportedMediaType, `{"code":3,"message":"OTLP JSON required"}`)
			return
		}
		encoding := strings.ToLower(strings.TrimSpace(strings.Join(r.Header.Values("Content-Encoding"), ",")))
		if encoding != "" && encoding != "identity" && encoding != "gzip" {
			claudeOTLPReply(w, http.StatusUnsupportedMediaType, `{"code":3,"message":"unsupported content encoding"}`)
			return
		}
		if r.ContentLength > claudeWireLimit {
			claudeOTLPTooLarge(w)
			return
		}
		var source io.Reader = http.MaxBytesReader(w, r.Body, claudeWireLimit)
		var compressed *gzip.Reader
		if encoding == "gzip" {
			compressed, err = gzip.NewReader(source)
			if err != nil {
				claudeOTLPReadError(w, err)
				return
			}
			source = io.LimitReader(compressed, claudeJSONLimit+1)
		}
		body, err := io.ReadAll(source)
		if compressed != nil {
			_ = compressed.Close()
		}
		if len(body) > claudeJSONLimit {
			claudeOTLPTooLarge(w)
			return
		}
		if err != nil {
			claudeOTLPReadError(w, err)
			return
		}

		var batch *claudeOTLPBatch
		if json.Unmarshal(body, &batch) != nil || batch == nil {
			claudeOTLPMalformed(w)
			return
		}
		for _, resource := range batch.ResourceLogs {
			var resourceIdentity claudeOTLPIdentity
			if !resourceIdentity.add(resource.Resource.Attributes) {
				continue
			}
			for _, scope := range resource.ScopeLogs {
				identity := resourceIdentity
				if !identity.add(scope.Scope.Attributes) {
					continue
				}
				for _, raw := range scope.LogRecords {
					obs, ok := claudeOTLPObservation(raw, identity)
					if ok && record(obs) != nil {
						// Earlier observations may already be durable. The writer deduplicates a replay.
						claudeOTLPReply(w, http.StatusServiceUnavailable, `{"code":14,"message":"desktop accounting temporarily unavailable"}`)
						return
					}
				}
			}
		}
		claudeOTLPReply(w, http.StatusOK, `{}`)
	})
}

func claudeOTLPReadError(w http.ResponseWriter, err error) {
	var limit *http.MaxBytesError
	if errors.As(err, &limit) {
		claudeOTLPTooLarge(w)
	} else {
		claudeOTLPMalformed(w)
	}
}

func claudeOTLPReply(w http.ResponseWriter, status int, body string) {
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func claudeOTLPTooLarge(w http.ResponseWriter) {
	claudeOTLPReply(w, http.StatusRequestEntityTooLarge, `{"code":8,"message":"OTLP body too large"}`)
}

func claudeOTLPMalformed(w http.ResponseWriter) {
	claudeOTLPReply(w, http.StatusBadRequest, `{"code":3,"message":"malformed OTLP JSON request"}`)
}

type claudeOTLPBatch struct {
	ResourceLogs []struct {
		Resource  claudeOTLPContext `json:"resource"`
		ScopeLogs []struct {
			Scope      claudeOTLPContext `json:"scope"`
			LogRecords []json.RawMessage `json:"logRecords"`
		} `json:"scopeLogs"`
	} `json:"resourceLogs"`
}

type claudeOTLPContext struct {
	Attributes []claudeOTLPAttribute `json:"attributes"`
}

type claudeOTLPAttribute struct {
	Key   string          `json:"key"`
	Value claudeOTLPValue `json:"value"`
}

type claudeOTLPValue struct {
	String json.RawMessage `json:"stringValue"`
	Int    json.RawMessage `json:"intValue"`
}

func (v claudeOTLPValue) text() (string, bool) {
	var s string
	ok := len(v.String) != 0 && v.String[0] == '"' && len(v.Int) == 0 && json.Unmarshal(v.String, &s) == nil
	return s, ok
}

type claudeOTLPIdentity struct {
	desktop bool
}

func (i *claudeOTLPIdentity) add(attrs []claudeOTLPAttribute) bool {
	for _, attr := range attrs {
		switch attr.Key {
		case "app.entrypoint":
			value, ok := attr.Value.text()
			if !ok || value != "claude-desktop" {
				return false
			}
			i.desktop = true
		case "provider", "api_provider", "gen_ai.provider.name", "gen_ai.system":
			value, ok := attr.Value.text()
			if !ok || (value != "anthropic" && value != "firstParty" && value != "first-party") {
				return false
			}
		}
	}
	return true
}

func claudeOTLPObservation(raw json.RawMessage, identity claudeOTLPIdentity) (Observation, bool) {
	var log struct {
		Name       json.RawMessage       `json:"eventName"`
		Body       claudeOTLPValue       `json:"body"`
		Time       json.RawMessage       `json:"timeUnixNano"`
		Attributes []claudeOTLPAttribute `json:"attributes"`
	}
	if json.Unmarshal(raw, &log) != nil || !identity.add(log.Attributes) || !identity.desktop {
		return Observation{}, false
	}
	var apiRequest bool
	if len(log.Name) != 0 {
		name, ok := (claudeOTLPValue{String: log.Name}).text()
		if !ok || (name != "" && !claudeAPIRequest(name)) {
			return Observation{}, false
		}
		apiRequest = name != ""
	}
	if body, ok := log.Body.text(); ok && claudeAPIRequest(body) {
		apiRequest = true
	}

	var id, model string
	var counts [4]int64
	var seen uint8
	for _, attr := range log.Attributes {
		var counter int
		switch attr.Key {
		case "event.name":
			name, ok := attr.Value.text()
			if !ok || !claudeAPIRequest(name) {
				return Observation{}, false
			}
			apiRequest = true
			continue
		case "request_id":
			value, ok := attr.Value.text()
			if !ok || len(value) > 128 || !claudeID.MatchString(value) || (id != "" && id != value) {
				return Observation{}, false
			}
			id = value
			continue
		case "model":
			value, ok := attr.Value.text()
			if ok && modelName.MatchString(value) {
				model = value
			}
			continue
		case "input_tokens":
			counter = 0
		case "output_tokens":
			counter = 1
		case "cache_read_tokens":
			counter = 2
		case "cache_creation_tokens":
			counter = 3
		default:
			continue
		}
		count, ok := claudeOTLPUint(attr.Value.Int)
		if !ok || len(attr.Value.String) != 0 || count > 1_000_000_000 || seen&(1<<counter) != 0 {
			return Observation{}, false
		}
		seen |= 1 << counter
		counts[counter] = int64(count)
	}
	if !apiRequest || id == "" || seen != 0b1111 {
		return Observation{}, false
	}
	nanos, ok := claudeOTLPUint(log.Time)
	if !ok || nanos == 0 {
		return Observation{}, false
	}
	timestamp := time.Unix(int64(nanos/1e9), int64(nanos%1e9)).UTC()
	if !validTime(timestamp) {
		return Observation{}, false
	}
	if model == "" {
		model = "unknown"
	}
	return Observation{
		Harness:          Claude,
		CanonicalID:      id,
		IdentityStrength: "request",
		Provider:         "anthropic",
		Model:            model,
		Timestamp:        timestamp,
		InputTokens:      counts[0],
		OutputTokens:     counts[1],
		CacheReadTokens:  counts[2],
		CacheWriteTokens: counts[3],
	}, true
}

func claudeAPIRequest(name string) bool {
	return name == "claude_code.api_request" || name == "api_request"
}

func claudeOTLPUint(raw json.RawMessage) (uint64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	text := string(raw)
	if raw[0] == '"' && json.Unmarshal(raw, &text) != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(text, 10, 64)
	return n, err == nil
}
