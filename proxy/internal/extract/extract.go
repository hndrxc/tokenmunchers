package extract

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"regexp"
	"strings"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/config"
)

const (
	// MaxJSONCapture limits buffering for non-streaming JSON responses.
	// Streaming SSE/NDJSON responses are parsed incrementally line-by-line with
	// constant memory so arbitrarily large streams never truncate trailing usage.
	MaxJSONCapture = 16 * 1024 * 1024
	maxLineBytes   = 2 * 1024 * 1024
	// maxUsageRecords bounds the records retained/emitted for one response
	// (e.g. distinct models in Anthropic billing iterations).
	maxUsageRecords = 32
	// maxResponseIDBytes bounds a retained provider response ID.
	maxResponseIDBytes = 256
)

var geminiModelPathRE = regexp.MustCompile(`/models/([A-Za-z0-9._-]+)(?::|$)`)
var safeModelRE = regexp.MustCompile(`[^A-Za-z0-9._:/@+-]`)

// UsageRecord represents one extracted model call (or Anthropic billing iteration).
type UsageRecord struct {
	ResponseID       string
	Model            string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReportedCostUSD  *float64
}

// TotalTokens returns the sum of the four disjoint token categories.
func (u UsageRecord) TotalTokens() int64 {
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// RequestHint holds minimal non-sensitive metadata peeked from the request
// (model name and URL path only — never prompts or content).
type RequestHint struct {
	Model string
	Path  string
}

// PeekRequestModel extracts only the top-level "model" string from a JSON request body
// or from a Gemini-style URL path (/models/<model>:generateContent).
func PeekRequestModel(path string, reqBody []byte) string {
	if len(reqBody) > 0 {
		var partial struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(reqBody, &partial); err == nil && partial.Model != "" {
			return SanitizeModel(partial.Model)
		}
	}
	if m := geminiModelPathRE.FindStringSubmatch(path); len(m) == 2 {
		return SanitizeModel(m[1])
	}
	return ""
}

// SanitizeModel cleans a model name to safe characters and caps its length at 128.
func SanitizeModel(model string) string {
	m := strings.TrimSpace(model)
	m = safeModelRE.ReplaceAllString(m, "")
	if len(m) > 128 {
		m = m[:128]
	}
	return m
}

// StreamMode classifies how a response body is framed.
type StreamMode int

const (
	ModeJSON StreamMode = iota
	ModeSSE
	ModeNDJSON
)

// DetectMode infers the response framing from Content-Type and format.
func DetectMode(contentType string, format config.Format) StreamMode {
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "text/event-stream") {
		return ModeSSE
	}
	if strings.Contains(ct, "application/x-ndjson") || strings.Contains(ct, "application/ndjson") {
		return ModeNDJSON
	}
	if format == config.FormatOllama && strings.Contains(ct, "application/json") {
		// Ollama streaming responses often use application/json with newline-delimited JSON objects.
		return ModeNDJSON
	}
	return ModeJSON
}

// Tap incrementally captures usage metadata from a proxied response stream
// without retaining prompts or response text.
type Tap struct {
	format config.Format
	isGzip bool
	mode   StreamMode
	hint   RequestHint

	// For incremental line-based parsing (uncompressed SSE / NDJSON).
	// lineBuf holds a partial line of at most maxLineBytes; discarding is set
	// while skipping the remainder of an oversized line up to its newline.
	lineBuf    []byte
	discarding bool
	// Compact streaming state for SSE/NDJSON so we don't retain content delta events!
	acc *streamAccumulator

	// For non-streaming JSON or gzipped streams
	rawBuf   bytes.Buffer
	overflow bool
}

// NewTap creates a Tap for a single proxied HTTP response.
func NewTap(format config.Format, contentType, contentEncoding string, hint RequestHint) *Tap {
	isGzip := strings.Contains(strings.ToLower(contentEncoding), "gzip")
	mode := DetectMode(contentType, format)
	return &Tap{
		format: format,
		isGzip: isGzip,
		mode:   mode,
		hint:   hint,
		acc:    newStreamAccumulator(format, hint),
	}
}

// Write feeds raw response bytes from the upstream into the tap.
// Always returns len(p), nil so it never interferes with proxying.
func (t *Tap) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if !t.isGzip && (t.mode == ModeSSE || t.mode == ModeNDJSON) {
		t.feedLines(p)
		return len(p), nil
	}
	// Buffer up to MaxJSONCapture for JSON or gzipped payloads.
	if !t.overflow {
		rem := MaxJSONCapture - t.rawBuf.Len()
		if len(p) <= rem {
			t.rawBuf.Write(p)
		} else {
			t.rawBuf.Write(p[:rem])
			t.overflow = true
		}
	}
	return len(p), nil
}

// feedLines splits p into newline-delimited lines and parses each complete line
// of at most maxLineBytes. A longer line is discarded in its entirety, through
// its terminating newline, however it is fragmented across writes; no part of
// it (including a suffix that happens to look like JSON) is ever parsed.
func (t *Tap) feedLines(p []byte) {
	for len(p) > 0 {
		var seg []byte
		complete := false
		if nl := bytes.IndexByte(p, '\n'); nl >= 0 {
			seg, p, complete = p[:nl], p[nl+1:], true
		} else {
			seg, p = p, nil
		}

		if t.discarding {
			if complete {
				t.discarding = false
			}
			continue
		}
		if len(t.lineBuf)+len(seg) > maxLineBytes {
			t.lineBuf = nil // release the buffered prefix of the oversized line
			t.discarding = !complete
			continue
		}
		if !complete {
			t.lineBuf = append(t.lineBuf, seg...)
			continue
		}
		if len(t.lineBuf) == 0 {
			t.processLine(seg)
			continue
		}
		t.lineBuf = append(t.lineBuf, seg...)
		t.processLine(t.lineBuf)
		t.lineBuf = t.lineBuf[:0]
	}
}

// flushPartial parses a trailing line that had no terminating newline, unless
// it was part of an oversized line being discarded.
func (t *Tap) flushPartial() {
	if len(t.lineBuf) > 0 && !t.discarding {
		t.processLine(t.lineBuf)
	}
	t.lineBuf = nil
	t.discarding = false
}

func (t *Tap) processLine(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	if t.mode == ModeSSE {
		if !bytes.HasPrefix(line, []byte("data:")) {
			return
		}
		line = bytes.TrimSpace(line[len("data:"):])
		if len(line) == 0 || bytes.Equal(line, []byte("[DONE]")) {
			return
		}
	}
	if line[0] != '{' {
		return
	}
	// Quick pre-filter: skip pure content-delta JSON objects that contain no usage/model/id fields
	// so long streams use negligible CPU and zero memory growth.
	if !mightContainUsageOrMeta(line) {
		return
	}
	t.acc.FeedObject(line)
}

func mightContainUsageOrMeta(line []byte) bool {
	return bytes.Contains(line, []byte(`"usage"`)) ||
		bytes.Contains(line, []byte(`"usageMetadata"`)) ||
		bytes.Contains(line, []byte(`"message_start"`)) ||
		bytes.Contains(line, []byte(`"message_delta"`)) ||
		bytes.Contains(line, []byte(`"message_stop"`)) ||
		bytes.Contains(line, []byte(`"response.completed"`)) ||
		bytes.Contains(line, []byte(`"response.incomplete"`)) ||
		bytes.Contains(line, []byte(`"prompt_eval_count"`)) ||
		bytes.Contains(line, []byte(`"eval_count"`)) ||
		bytes.Contains(line, []byte(`"modelVersion"`))
}

// Finalize completes parsing and returns zero or more UsageRecords.
// clientDisconnected should be true if the downstream client broke the connection
// before the upstream response finished.
func (t *Tap) Finalize(clientDisconnected bool) []UsageRecord {
	if !t.isGzip && (t.mode == ModeSSE || t.mode == ModeNDJSON) {
		t.flushPartial()
		return t.acc.Finalize(clientDisconnected)
	}

	raw := t.rawBuf.Bytes()
	if len(raw) == 0 {
		return nil
	}
	if t.isGzip {
		gr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil
		}
		defer gr.Close()
		decompressed, err := io.ReadAll(io.LimitReader(gr, MaxJSONCapture))
		if err != nil && len(decompressed) == 0 {
			return nil
		}
		raw = decompressed
	}

	if t.mode == ModeSSE || t.mode == ModeNDJSON || looksLikeSSE(raw) {
		// Decompressed framing: SSE if it looks like SSE; a declared NDJSON
		// stream (or SSE-typed Ollama body) stays line-delimited JSON.
		switch {
		case looksLikeSSE(raw):
			t.mode = ModeSSE
		case t.mode == ModeSSE && t.format == config.FormatOllama:
			t.mode = ModeNDJSON
		}
		t.feedLines(raw)
		t.flushPartial()
		return t.acc.Finalize(clientDisconnected)
	}

	// Non-streaming single JSON response (or JSON array for Gemini batch responses).
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err == nil {
			for _, item := range items {
				t.acc.FeedObject(item)
			}
			return t.acc.Finalize(clientDisconnected)
		}
		return nil
	}
	if trimmed[0] == '{' {
		t.acc.FeedObject(trimmed)
		return t.acc.Finalize(clientDisconnected)
	}
	return nil
}

func looksLikeSSE(b []byte) bool {
	head := b
	if len(head) > 256 {
		head = head[:256]
	}
	return bytes.Contains(head, []byte("data:")) || bytes.Contains(head, []byte("event:"))
}

// streamAccumulator maintains minimal state across one or more JSON objects in a response.
type streamAccumulator struct {
	format config.Format
	hint   RequestHint

	anthropic *anthropicAcc
	openai    *openaiAcc
	gemini    *geminiAcc
	ollama    *ollamaAcc
}

func newStreamAccumulator(format config.Format, hint RequestHint) *streamAccumulator {
	return &streamAccumulator{
		format:    format,
		hint:      hint,
		anthropic: newAnthropicAcc(hint),
		openai:    newOpenAIAcc(hint),
		gemini:    newGeminiAcc(hint),
		ollama:    newOllamaAcc(hint),
	}
}

func (a *streamAccumulator) FeedObject(raw []byte) {
	switch a.format {
	case config.FormatAnthropic:
		a.anthropic.Feed(raw)
	case config.FormatOpenAI:
		a.openai.Feed(raw)
	case config.FormatGemini:
		a.gemini.Feed(raw)
	case config.FormatOllama:
		a.ollama.Feed(raw)
		a.openai.Feed(raw) // Ollama also serves OpenAI-compatible /v1/chat/completions
	default: // FormatAuto
		a.anthropic.Feed(raw)
		a.openai.Feed(raw)
		a.gemini.Feed(raw)
		a.ollama.Feed(raw)
	}
}

func (a *streamAccumulator) Finalize(clientDisconnected bool) []UsageRecord {
	var records []UsageRecord
	switch a.format {
	case config.FormatAnthropic:
		records = a.anthropic.Finalize(clientDisconnected)
	case config.FormatOpenAI:
		records = a.openai.Finalize()
	case config.FormatGemini:
		records = a.gemini.Finalize()
	case config.FormatOllama:
		records = a.ollama.Finalize()
		if len(records) == 0 {
			records = a.openai.Finalize()
		}
	default:
		if r := a.anthropic.Finalize(clientDisconnected); len(r) > 0 {
			records = r
		} else if r := a.openai.Finalize(); len(r) > 0 {
			records = r
		} else if r := a.gemini.Finalize(); len(r) > 0 {
			records = r
		} else if r := a.ollama.Finalize(); len(r) > 0 {
			records = r
		}
	}

	// Filter out zero-token records and ensure models are sanitized.
	out := make([]UsageRecord, 0, len(records))
	for _, r := range records {
		if r.TotalTokens() <= 0 {
			continue
		}
		if r.Model == "" {
			r.Model = a.hint.Model
		}
		r.Model = SanitizeModel(r.Model)
		if r.Model == "" {
			r.Model = "unknown"
		}
		out = append(out, r)
	}
	return out
}

// boundID caps a provider-supplied identifier retained for storage.
func boundID(id string) string {
	if len(id) <= maxResponseIDBytes {
		return id
	}
	return strings.ToValidUTF8(id[:maxResponseIDBytes], "")
}

func nonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}
