package extract

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/tokenmunchers/tokenmunchers/proxy/internal/config"
)

func TestAnthropicSSECompleteAndCutShort(t *testing.T) {
	sse := "" +
		"event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_01\",\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":120,\"output_tokens\":1,\"cache_read_input_tokens\":400,\"cache_creation_input_tokens\":50}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hello world\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":85}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	tap := NewTap(config.FormatAnthropic, "text/event-stream", "", RequestHint{})
	_, _ = tap.Write([]byte(sse))
	recs := tap.Finalize(false)
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	r := recs[0]
	if r.ResponseID != "msg_01" || r.Model != "claude-sonnet-4-6" || r.InputTokens != 120 || r.OutputTokens != 85 || r.CacheReadTokens != 400 || r.CacheWriteTokens != 50 {
		t.Fatalf("unexpected record: %+v", r)
	}

	// Cut short after message_start: placeholder output_tokens=1 must be zeroed.
	shortSSE := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_02\",\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1,\"cache_read_input_tokens\":5,\"cache_creation_input_tokens\":3}}}\n\n"
	tap2 := NewTap(config.FormatAnthropic, "text/event-stream", "", RequestHint{})
	_, _ = tap2.Write([]byte(shortSSE))
	recs2 := tap2.Finalize(true)
	if len(recs2) != 1 || recs2[0].OutputTokens != 0 || recs2[0].InputTokens != 10 {
		t.Fatalf("expected placeholder output zeroed on cut stream, got %+v", recs2)
	}
}

func TestAnthropicMultiIteration(t *testing.T) {
	payload := `{"id":"msg_iter","type":"message","model":"claude-opus-4-6","usage":{"input_tokens":14,"output_tokens":17,"iterations":[{"type":"message","model":"claude-opus-4-6","input_tokens":10,"output_tokens":11,"cache_read_input_tokens":5,"cache_creation_input_tokens":0},{"type":"compaction","model":"claude-haiku-4-5","input_tokens":2,"output_tokens":3,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},{"type":"message","model":"claude-opus-4-6","input_tokens":2,"output_tokens":3,"cache_read_input_tokens":1,"cache_creation_input_tokens":0}]}}`

	tap := NewTap(config.FormatAnthropic, "application/json", "", RequestHint{})
	_, _ = tap.Write([]byte(payload))
	recs := tap.Finalize(false)
	if len(recs) != 2 {
		t.Fatalf("expected 2 per-model iteration records, got %d (%+v)", len(recs), recs)
	}
	if recs[0].Model != "claude-opus-4-6" || recs[0].InputTokens != 12 || recs[0].OutputTokens != 14 || recs[0].CacheReadTokens != 6 {
		t.Fatalf("unexpected opus record: %+v", recs[0])
	}
	if recs[1].Model != "claude-haiku-4-5" || recs[1].InputTokens != 2 || recs[1].OutputTokens != 3 {
		t.Fatalf("unexpected haiku record: %+v", recs[1])
	}
}

func TestOpenAIChatAndResponsesAndOpenRouterAndXAI(t *testing.T) {
	// OpenAI Responses SSE (Codex/ChatGPT format)
	respSSE := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_123\",\"model\":\"gpt-5.2-codex\",\"usage\":{\"input_tokens\":1000,\"output_tokens\":50,\"input_tokens_details\":{\"cached_tokens\":400},\"output_tokens_details\":{\"reasoning_tokens\":20}}}}\n\n"
	tap := NewTap(config.FormatOpenAI, "text/event-stream", "", RequestHint{})
	_, _ = tap.Write([]byte(respSSE))
	recs := tap.Finalize(false)
	if len(recs) != 1 {
		t.Fatalf("expected 1 responses record, got %d", len(recs))
	}
	if recs[0].ResponseID != "resp_123" || recs[0].InputTokens != 600 || recs[0].CacheReadTokens != 400 || recs[0].OutputTokens != 50 {
		t.Fatalf("unexpected responses record: %+v", recs[0])
	}

	// OpenRouter with reported usage.cost and cache_write_tokens
	orJSON := `{"id":"gen-1","model":"anthropic/claude-sonnet-4","usage":{"prompt_tokens":500,"completion_tokens":100,"prompt_tokens_details":{"cached_tokens":200,"cache_write_tokens":100},"cost":0.012345}}`
	tap2 := NewTap(config.FormatOpenAI, "application/json", "", RequestHint{})
	_, _ = tap2.Write([]byte(orJSON))
	recs2 := tap2.Finalize(false)
	if len(recs2) != 1 || recs2[0].InputTokens != 200 || recs2[0].CacheReadTokens != 200 || recs2[0].CacheWriteTokens != 100 || recs2[0].OutputTokens != 100 {
		t.Fatalf("unexpected OpenRouter record: %+v", recs2)
	}
	if recs2[0].ReportedCostUSD == nil || math.Abs(*recs2[0].ReportedCostUSD-0.012345) > 1e-9 {
		t.Fatalf("expected reported cost 0.012345, got %v", recs2[0].ReportedCostUSD)
	}

	// xAI with reasoning excluded from completion_tokens + cost_in_usd_ticks
	xaiJSON := `{"id":"xai-1","model":"grok-4","usage":{"prompt_tokens":32,"completion_tokens":9,"total_tokens":151,"prompt_tokens_details":{"cached_tokens":8},"completion_tokens_details":{"reasoning_tokens":110},"cost_in_usd_ticks":25000000}}`
	tap3 := NewTap(config.FormatOpenAI, "application/json", "", RequestHint{})
	_, _ = tap3.Write([]byte(xaiJSON))
	recs3 := tap3.Finalize(false)
	if len(recs3) != 1 || recs3[0].InputTokens != 24 || recs3[0].CacheReadTokens != 8 || recs3[0].OutputTokens != 119 {
		t.Fatalf("unexpected xAI record: %+v", recs3)
	}
	if recs3[0].ReportedCostUSD == nil || math.Abs(*recs3[0].ReportedCostUSD-0.0025) > 1e-9 {
		t.Fatalf("expected xAI cost 0.0025, got %v", recs3[0].ReportedCostUSD)
	}
}

func TestGeminiAndOllamaAndGzip(t *testing.T) {
	// Gemini SSE with model from path hint
	gemSSE := "data: {\"responseId\":\"g-1\",\"usageMetadata\":{\"promptTokenCount\":200,\"toolUsePromptTokenCount\":50,\"cachedContentTokenCount\":100,\"candidatesTokenCount\":40,\"thoughtsTokenCount\":30}}\n\n"
	hint := RequestHint{Model: PeekRequestModel("/v1beta/models/gemini-2.5-pro:streamGenerateContent", nil)}
	tap := NewTap(config.FormatGemini, "text/event-stream", "", hint)
	_, _ = tap.Write([]byte(gemSSE))
	recs := tap.Finalize(false)
	if len(recs) != 1 || recs[0].Model != "gemini-2.5-pro" || recs[0].InputTokens != 150 || recs[0].CacheReadTokens != 100 || recs[0].OutputTokens != 70 {
		t.Fatalf("unexpected Gemini record: %+v", recs)
	}

	// Ollama NDJSON
	ollamaStream := "{\"model\":\"llama3.2\",\"response\":\"hi\",\"done\":false}\n{\"model\":\"llama3.2\",\"done\":true,\"prompt_eval_count\":100,\"prompt_eval_cached_count\":20,\"eval_count\":45}\n"
	tap2 := NewTap(config.FormatOllama, "application/x-ndjson", "", RequestHint{})
	_, _ = tap2.Write([]byte(ollamaStream))
	recs2 := tap2.Finalize(false)
	if len(recs2) != 1 || recs2[0].Model != "llama3.2" || recs2[0].InputTokens != 80 || recs2[0].CacheReadTokens != 20 || recs2[0].OutputTokens != 45 || recs2[0].ReportedCostUSD == nil || *recs2[0].ReportedCostUSD != 0 {
		t.Fatalf("unexpected Ollama record: %+v", recs2)
	}

	// Gzipped OpenAI JSON
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	_, _ = gw.Write([]byte(`{"id":"gz-1","model":"gpt-4o","usage":{"prompt_tokens":50,"completion_tokens":25}}`))
	_ = gw.Close()
	tap3 := NewTap(config.FormatOpenAI, "application/json", "gzip", RequestHint{})
	_, _ = tap3.Write(gzBuf.Bytes())
	recs3 := tap3.Finalize(false)
	if len(recs3) != 1 || recs3[0].InputTokens != 50 || recs3[0].OutputTokens != 25 {
		t.Fatalf("unexpected gzipped OpenAI record: %+v", recs3)
	}
}

const usageLine = `{"id":"r1","model":"gpt-4o","usage":{"prompt_tokens":7,"completion_tokens":3}}`

// paddedUsageLine returns a valid JSON usage line of exactly n bytes.
func paddedUsageLine(n int) string {
	base := `{"pad":"","id":"r1","model":"gpt-4o","usage":{"prompt_tokens":7,"completion_tokens":3}}`
	return strings.Replace(base, `"pad":""`, `"pad":"`+strings.Repeat("A", n-len(base))+`"`, 1)
}

func tapRecords(t *testing.T, disconnected bool, writes ...string) []UsageRecord {
	t.Helper()
	tap := NewTap(config.FormatOpenAI, "application/x-ndjson", "", RequestHint{})
	for _, w := range writes {
		if n, err := tap.Write([]byte(w)); err != nil || n != len(w) {
			t.Fatalf("Write must never fail or short-write: n=%d err=%v", n, err)
		}
	}
	return tap.Finalize(disconnected)
}

func TestOversizedLineFragmentedSuffixNeverParsed(t *testing.T) {
	// The first fragment overruns the line limit; the second fragment is a
	// complete, valid usage object that is only the *suffix* of the same line.
	recs := tapRecords(t, false,
		"{\"pad\":\""+strings.Repeat("A", maxLineBytes+1),
		usageLine+"\n",
	)
	if len(recs) != 0 {
		t.Fatalf("suffix of an oversized line must not be parsed, got %+v", recs)
	}

	// Suffix delivered across several fragments, newline last.
	recs = tapRecords(t, false,
		strings.Repeat("A", maxLineBytes+1),
		usageLine,
		usageLine,
		"\n",
	)
	if len(recs) != 0 {
		t.Fatalf("multi-fragment suffix must not be parsed, got %+v", recs)
	}

	// Trailing suffix with no newline at all (flushed by Finalize).
	recs = tapRecords(t, false, strings.Repeat("A", maxLineBytes+1), usageLine)
	if len(recs) != 0 {
		t.Fatalf("unterminated suffix must not be parsed on Finalize, got %+v", recs)
	}
}

func TestOversizedLineRecoversAtNewline(t *testing.T) {
	recs := tapRecords(t, false,
		strings.Repeat("A", maxLineBytes/2),
		strings.Repeat("A", maxLineBytes/2+10),
		usageLine+"\n"+usageLine+"\n",
	)
	// First usageLine is the discarded line's tail; the second is a fresh line.
	if len(recs) != 1 || recs[0].InputTokens != 7 || recs[0].OutputTokens != 3 {
		t.Fatalf("expected parsing to resume after the oversized line, got %+v", recs)
	}
}

func TestSingleChunkOversizedLineDiscarded(t *testing.T) {
	line := paddedUsageLine(maxLineBytes + 1)
	if recs := tapRecords(t, false, line+"\n"); len(recs) != 0 {
		t.Fatalf("oversized single-chunk line must be discarded, got %+v", recs)
	}
	// Terminated line followed by more content in the same chunk.
	if recs := tapRecords(t, false, line+"\n"+usageLine+"\n"); len(recs) != 1 {
		t.Fatalf("line after oversized single-chunk line must still parse, got %+v", recs)
	}
	// Oversized because of buffered prefix plus a chunk that ends the line.
	half := paddedUsageLine(maxLineBytes/2 + 100)
	if recs := tapRecords(t, false, half[:len(half)-10], half[len(half)-10:]+half+"\n"); len(recs) != 0 {
		t.Fatalf("prefix+tail beyond the limit must be discarded, got %+v", recs)
	}
}

func TestLineAtLimitStillParsed(t *testing.T) {
	line := paddedUsageLine(maxLineBytes)
	if len(line) != maxLineBytes {
		t.Fatalf("test setup: line is %d bytes", len(line))
	}
	for name, writes := range map[string][]string{
		"single":     {line + "\n"},
		"fragmented": {line[:1000], line[1000:] + "\n"},
		"unflushed":  {line},
	} {
		recs := tapRecords(t, false, writes...)
		if len(recs) != 1 || recs[0].InputTokens != 7 || recs[0].OutputTokens != 3 {
			t.Fatalf("%s: line at the limit must parse, got %+v", name, recs)
		}
	}
}

func TestOversizedLineInGzippedStreamDiscarded(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, _ = gw.Write([]byte(paddedUsageLine(maxLineBytes+1) + "\n" + usageLine + "\n"))
	_ = gw.Close()
	tap := NewTap(config.FormatOpenAI, "application/x-ndjson", "gzip", RequestHint{})
	_, _ = tap.Write(buf.Bytes())
	recs := tap.Finalize(false)
	if len(recs) != 1 || recs[0].InputTokens != 7 {
		t.Fatalf("expected only the in-bounds line from the gzipped stream, got %+v", recs)
	}
}

func TestAnthropicIterationRecordsBounded(t *testing.T) {
	var sb strings.Builder
	const n = 500
	sb.WriteString(`{"id":"msg_many","type":"message","model":"base","usage":{"input_tokens":1,"output_tokens":1,"iterations":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"type":"message","model":"m-%d","input_tokens":2,"output_tokens":3,"cache_read_input_tokens":1,"cache_creation_input_tokens":1}`, i)
	}
	sb.WriteString(`,{"type":"message","input_tokens":5,"output_tokens":0}]}}`)

	tap := NewTap(config.FormatAnthropic, "application/json", "", RequestHint{})
	_, _ = tap.Write([]byte(sb.String()))
	recs := tap.Finalize(false)
	if len(recs) == 0 || len(recs) > maxUsageRecords {
		t.Fatalf("records must be bounded by %d, got %d", maxUsageRecords, len(recs))
	}
	var in, out, cr, cw int64
	for _, r := range recs {
		in += r.InputTokens
		out += r.OutputTokens
		cr += r.CacheReadTokens
		cw += r.CacheWriteTokens
	}
	if in != 2*n+5 || out != 3*n || cr != n || cw != n {
		t.Fatalf("overflow must preserve token totals: in=%d out=%d cr=%d cw=%d", in, out, cr, cw)
	}
	if recs[0].Model != "m-0" || recs[len(recs)-1].Model != overflowModel {
		t.Fatalf("expected first-seen order with overflow bucket last, got first=%q last=%q", recs[0].Model, recs[len(recs)-1].Model)
	}
}

func TestResponseIDBounded(t *testing.T) {
	payload := `{"id":"` + strings.Repeat("x", 10_000) + `","model":"gpt-4o","usage":{"prompt_tokens":1,"completion_tokens":1}}`
	tap := NewTap(config.FormatOpenAI, "application/json", "", RequestHint{})
	_, _ = tap.Write([]byte(payload))
	recs := tap.Finalize(false)
	if len(recs) != 1 || len(recs[0].ResponseID) != maxResponseIDBytes {
		t.Fatalf("expected response ID capped at %d bytes, got %+v", maxResponseIDBytes, recs)
	}
}

func TestBufferedJSONCaptureLimitPreserved(t *testing.T) {
	tap := NewTap(config.FormatOpenAI, "application/json", "", RequestHint{})
	chunk := bytes.Repeat([]byte("A"), 1<<20)
	for i := 0; i < MaxJSONCapture/len(chunk)+4; i++ {
		if n, err := tap.Write(chunk); err != nil || n != len(chunk) {
			t.Fatalf("Write must keep forwarding past the capture limit: n=%d err=%v", n, err)
		}
	}
	if tap.rawBuf.Len() != MaxJSONCapture {
		t.Fatalf("buffered capture must stop at MaxJSONCapture, got %d", tap.rawBuf.Len())
	}
	if recs := tap.Finalize(false); len(recs) != 0 {
		t.Fatalf("unexpected records: %+v", recs)
	}
}
