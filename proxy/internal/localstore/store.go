package localstore

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Record is a metadata-only local usage entry.
// Never contains prompts, responses, headers, credentials, or file paths.
type Record struct {
	EventID          string  `json:"event_id"`
	Timestamp        string  `json:"ts"`
	Route            string  `json:"route"`
	Provider         string  `json:"provider"`
	Model            string  `json:"model"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	CostEstimated    bool    `json:"cost_estimated,omitempty"`
}

// TotalTokens returns the sum of the four disjoint token categories.
func (r Record) TotalTokens() int64 {
	return r.InputTokens + r.OutputTokens + r.CacheReadTokens + r.CacheWriteTokens
}

// ModelSummary aggregates usage for one (provider, model) pair.
type ModelSummary struct {
	Provider         string
	Model            string
	Calls            int64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	TotalTokens      int64
	CostUSD          float64
}

// Summary aggregates local usage over a time window.
type Summary struct {
	Period           string
	Calls            int64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	TotalTokens      int64
	CostUSD          float64
	ByModel          []ModelSummary
}

// Store appends metadata-only records to a local JSONL log.
type Store struct {
	mu   sync.Mutex
	path string
}

// New creates a Store backed by path.
func New(path string) *Store {
	return &Store{path: path}
}

// Append writes a single record to the local usage log.
func (s *Store) Append(rec Record) error {
	if s == nil || s.path == "" {
		return nil
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(line)
	return err
}

// Summarize reads the local log and computes totals for "today", "week", or "all" (UTC).
func (s *Store) Summarize(period string, now time.Time) (Summary, error) {
	if period == "" {
		period = "today"
	}
	out := Summary{Period: period}
	if s == nil || s.path == "" {
		return out, nil
	}

	var cutoff time.Time
	nowUTC := now.UTC()
	switch period {
	case "today":
		cutoff = time.Date(nowUTC.Year(), nowUTC.Month(), nowUTC.Day(), 0, 0, 0, 0, time.UTC)
	case "week":
		cutoff = nowUTC.Add(-7 * 24 * time.Hour)
	case "all":
		cutoff = time.Time{}
	default:
		return out, fmt.Errorf("unknown period %q (expected today, week, or all)", period)
	}

	f, err := os.Open(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return out, err
	}
	defer f.Close()

	type modelKey struct{ provider, model string }
	byModel := make(map[modelKey]*ModelSummary)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if !cutoff.IsZero() {
			ts, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
			if err != nil || ts.Before(cutoff) {
				continue
			}
		}
		tot := rec.TotalTokens()
		out.Calls++
		out.InputTokens += rec.InputTokens
		out.OutputTokens += rec.OutputTokens
		out.CacheReadTokens += rec.CacheReadTokens
		out.CacheWriteTokens += rec.CacheWriteTokens
		out.TotalTokens += tot
		out.CostUSD += rec.CostUSD

		k := modelKey{provider: rec.Provider, model: rec.Model}
		ms := byModel[k]
		if ms == nil {
			ms = &ModelSummary{Provider: rec.Provider, Model: rec.Model}
			byModel[k] = ms
		}
		ms.Calls++
		ms.InputTokens += rec.InputTokens
		ms.OutputTokens += rec.OutputTokens
		ms.CacheReadTokens += rec.CacheReadTokens
		ms.CacheWriteTokens += rec.CacheWriteTokens
		ms.TotalTokens += tot
		ms.CostUSD += rec.CostUSD
	}
	if err := scanner.Err(); err != nil {
		return out, err
	}

	out.ByModel = make([]ModelSummary, 0, len(byModel))
	for _, ms := range byModel {
		out.ByModel = append(out.ByModel, *ms)
	}
	sort.Slice(out.ByModel, func(i, j int) bool {
		if out.ByModel[i].TotalTokens != out.ByModel[j].TotalTokens {
			return out.ByModel[i].TotalTokens > out.ByModel[j].TotalTokens
		}
		return out.ByModel[i].Model < out.ByModel[j].Model
	})
	return out, nil
}
