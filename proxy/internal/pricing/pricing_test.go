package pricing

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestLookupAndEstimate(t *testing.T) {
	tbl := NewTable(nil)

	// Dated Claude model should match claude-sonnet-4-6 (3/15/0.30/3.75 per 1M).
	cost, ok := tbl.Estimate("claude-sonnet-4-6-20260514", 1_000_000, 100_000, 500_000, 200_000)
	if !ok {
		t.Fatal("expected claude-sonnet-4-6 match")
	}
	want := 3.0 + 1.5 + 0.15 + 0.75 // 5.40
	if math.Abs(cost-want) > 1e-6 {
		t.Fatalf("got cost %v, want %v", cost, want)
	}

	// Provider-prefixed model should strip prefix.
	cost2, ok := tbl.Estimate("anthropic/claude-opus-4-6", 1_000_000, 0, 0, 0)
	if !ok || math.Abs(cost2-15.0) > 1e-6 {
		t.Fatalf("expected 15.0 for anthropic/claude-opus-4-6, got %v (ok=%v)", cost2, ok)
	}

	// Gemini models/ prefix and specific flash-lite over flash.
	cost3, ok := tbl.Estimate("models/gemini-2.5-flash-lite-preview", 1_000_000, 0, 0, 0)
	if !ok || math.Abs(cost3-0.10) > 1e-6 {
		t.Fatalf("expected 0.10 for gemini-2.5-flash-lite, got %v (ok=%v)", cost3, ok)
	}

	// Unknown model returns (0, false).
	if c, ok := tbl.Estimate("totally-unknown-model-xyz", 1000, 1000, 0, 0); ok || c != 0 {
		t.Fatalf("expected (0, false) for unknown model, got (%v, %v)", c, ok)
	}
}

func TestLoadTableOverrides(t *testing.T) {
	p := filepath.Join(t.TempDir(), "prices.json")
	if err := os.WriteFile(p, []byte(`{"my-model":{"input":5,"output":20,"cache_read":0.5,"cache_write":6}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tbl, err := LoadTable(p)
	if err != nil {
		t.Fatalf("LoadTable failed: %v", err)
	}
	cost, ok := tbl.Estimate("my-model-v2", 1_000_000, 500_000, 0, 0)
	if !ok || math.Abs(cost-15.0) > 1e-6 {
		t.Fatalf("expected 15.0, got %v (ok=%v)", cost, ok)
	}
}
