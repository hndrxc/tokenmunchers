package localstore

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreAppendAndSummarize(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "usage.jsonl"))
	now := time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)

	records := []Record{
		{
			EventID: "1", Timestamp: now.Add(-1 * time.Hour).Format("2006-01-02T15:04:05.000Z"),
			Route: "anthropic", Provider: "anthropic", Model: "claude-sonnet-4-6",
			InputTokens: 100, OutputTokens: 50, CacheReadTokens: 200, CostUSD: 0.01,
		},
		{
			EventID: "2", Timestamp: now.Add(-3 * 24 * time.Hour).Format("2006-01-02T15:04:05.000Z"),
			Route: "openai", Provider: "openai", Model: "gpt-5.2",
			InputTokens: 500, OutputTokens: 100, CostUSD: 0.02,
		},
		{
			EventID: "3", Timestamp: now.Add(-15 * 24 * time.Hour).Format("2006-01-02T15:04:05.000Z"),
			Route: "gemini", Provider: "google", Model: "gemini-2.5-pro",
			InputTokens: 1000, OutputTokens: 200, CostUSD: 0.03,
		},
	}
	for _, r := range records {
		if err := store.Append(r); err != nil {
			t.Fatal(err)
		}
	}

	today, err := store.Summarize("today", now)
	if err != nil || today.Calls != 1 || today.TotalTokens != 350 {
		t.Fatalf("unexpected today summary: %+v (err=%v)", today, err)
	}
	week, err := store.Summarize("week", now)
	if err != nil || week.Calls != 2 || week.TotalTokens != 950 {
		t.Fatalf("unexpected week summary: %+v (err=%v)", week, err)
	}
	all, err := store.Summarize("all", now)
	if err != nil || all.Calls != 3 || all.TotalTokens != 2150 {
		t.Fatalf("unexpected all summary: %+v (err=%v)", all, err)
	}
}
