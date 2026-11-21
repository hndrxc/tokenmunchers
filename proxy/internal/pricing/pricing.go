package pricing

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"strings"
)

// Rate holds per-1M-token USD pricing for a model.
type Rate struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// Table maps model prefixes to USD per 1M token rates.
// Longer prefixes take priority so specific models override family defaults.
type Table struct {
	entries []entry
}

type entry struct {
	prefix string
	rate   Rate
}

// DefaultRates lists list-price estimates (USD per 1M tokens) for common models.
// Users can extend or override any entry via prices.json in their config directory.
func DefaultRates() map[string]Rate {
	return map[string]Rate{
		// Anthropic Claude
		"claude-opus-4-6":   {Input: 15.0, Output: 75.0, CacheRead: 1.50, CacheWrite: 18.75},
		"claude-opus-4-5":   {Input: 15.0, Output: 75.0, CacheRead: 1.50, CacheWrite: 18.75},
		"claude-opus-4-1":   {Input: 15.0, Output: 75.0, CacheRead: 1.50, CacheWrite: 18.75},
		"claude-opus-4":     {Input: 15.0, Output: 75.0, CacheRead: 1.50, CacheWrite: 18.75},
		"claude-sonnet-4-6": {Input: 3.0, Output: 15.0, CacheRead: 0.30, CacheWrite: 3.75},
		"claude-sonnet-4-5": {Input: 3.0, Output: 15.0, CacheRead: 0.30, CacheWrite: 3.75},
		"claude-sonnet-4":   {Input: 3.0, Output: 15.0, CacheRead: 0.30, CacheWrite: 3.75},
		"claude-3-7-sonnet": {Input: 3.0, Output: 15.0, CacheRead: 0.30, CacheWrite: 3.75},
		"claude-3-5-sonnet": {Input: 3.0, Output: 15.0, CacheRead: 0.30, CacheWrite: 3.75},
		"claude-haiku-4-5":  {Input: 1.0, Output: 5.0, CacheRead: 0.10, CacheWrite: 1.25},
		"claude-3-5-haiku":  {Input: 0.80, Output: 4.0, CacheRead: 0.08, CacheWrite: 1.00},
		"claude-3-haiku":    {Input: 0.25, Output: 1.25, CacheRead: 0.03, CacheWrite: 0.30},
		"claude-3-opus":     {Input: 15.0, Output: 75.0, CacheRead: 1.50, CacheWrite: 18.75},

		// OpenAI
		"gpt-5.4":       {Input: 2.50, Output: 15.0, CacheRead: 0.25},
		"gpt-5.3-codex": {Input: 1.75, Output: 14.0, CacheRead: 0.175},
		"gpt-5.2-codex": {Input: 1.75, Output: 14.0, CacheRead: 0.175},
		"gpt-5.2":       {Input: 1.75, Output: 14.0, CacheRead: 0.175},
		"gpt-5.1-codex": {Input: 1.25, Output: 10.0, CacheRead: 0.125},
		"gpt-5.1":       {Input: 1.25, Output: 10.0, CacheRead: 0.125},
		"gpt-5-codex":   {Input: 1.25, Output: 10.0, CacheRead: 0.125},
		"gpt-5-mini":    {Input: 0.25, Output: 2.00, CacheRead: 0.025},
		"gpt-5-nano":    {Input: 0.05, Output: 0.40, CacheRead: 0.005},
		"gpt-5":         {Input: 1.25, Output: 10.0, CacheRead: 0.125},
		"gpt-4.1-mini":  {Input: 0.40, Output: 1.60, CacheRead: 0.10},
		"gpt-4.1-nano":  {Input: 0.10, Output: 0.40, CacheRead: 0.025},
		"gpt-4.1":       {Input: 2.00, Output: 8.00, CacheRead: 0.50},
		"gpt-4o-mini":   {Input: 0.15, Output: 0.60, CacheRead: 0.075},
		"gpt-4o":        {Input: 2.50, Output: 10.0, CacheRead: 1.25},
		"o4-mini":       {Input: 1.10, Output: 4.40, CacheRead: 0.275},
		"o3-mini":       {Input: 1.10, Output: 4.40, CacheRead: 0.55},
		"o3-pro":        {Input: 20.0, Output: 80.0, CacheRead: 5.00},
		"o3":            {Input: 2.00, Output: 8.00, CacheRead: 0.50},
		"o1-mini":       {Input: 1.10, Output: 4.40, CacheRead: 0.55},
		"o1":            {Input: 15.0, Output: 60.0, CacheRead: 7.50},

		// Google Gemini
		"gemini-3-pro":         {Input: 2.00, Output: 12.0, CacheRead: 0.20},
		"gemini-3-flash":       {Input: 0.50, Output: 3.00, CacheRead: 0.05},
		"gemini-2.5-pro":       {Input: 1.25, Output: 10.0, CacheRead: 0.31},
		"gemini-2.5-flash-lite": {Input: 0.10, Output: 0.40, CacheRead: 0.025},
		"gemini-2.5-flash":     {Input: 0.30, Output: 2.50, CacheRead: 0.075},
		"gemini-2.0-flash-lite": {Input: 0.075, Output: 0.30, CacheRead: 0.01875},
		"gemini-2.0-flash":     {Input: 0.10, Output: 0.40, CacheRead: 0.025},

		// xAI Grok
		"grok-4-fast":   {Input: 0.20, Output: 0.50, CacheRead: 0.05},
		"grok-4":        {Input: 3.00, Output: 15.0, CacheRead: 0.75},
		"grok-code-fast": {Input: 0.20, Output: 1.50, CacheRead: 0.02},
		"grok-3-mini":   {Input: 0.30, Output: 0.50, CacheRead: 0.075},
		"grok-3":        {Input: 3.00, Output: 15.0, CacheRead: 0.75},

		// DeepSeek
		"deepseek-reasoner": {Input: 0.55, Output: 2.19, CacheRead: 0.14},
		"deepseek-r1":       {Input: 0.55, Output: 2.19, CacheRead: 0.14},
		"deepseek-chat":     {Input: 0.27, Output: 1.10, CacheRead: 0.07},
		"deepseek-v3":       {Input: 0.27, Output: 1.10, CacheRead: 0.07},
	}
}

// NewTable builds a pricing table from DefaultRates plus optional overrides.
func NewTable(overrides map[string]Rate) *Table {
	merged := DefaultRates()
	for k, v := range overrides {
		k = normalizeKey(k)
		if k != "" {
			merged[k] = v
		}
	}
	entries := make([]entry, 0, len(merged))
	for k, v := range merged {
		entries = append(entries, entry{prefix: k, rate: v})
	}
	// Sort longest prefix first for deterministic most-specific matching.
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			if len(entries[j].prefix) > len(entries[i].prefix) ||
				(len(entries[j].prefix) == len(entries[i].prefix) && entries[j].prefix < entries[i].prefix) {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}
	return &Table{entries: entries}
}

// LoadTable loads DefaultRates merged with an optional JSON overrides file.
// Missing file is not an error.
func LoadTable(path string) (*Table, error) {
	if path == "" {
		return NewTable(nil), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return NewTable(nil), nil
		}
		return nil, err
	}
	var overrides map[string]Rate
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &overrides); err != nil {
			return nil, err
		}
	}
	return NewTable(overrides), nil
}

// Lookup finds the most specific matching rate for a model name.
func (t *Table) Lookup(model string) (Rate, bool) {
	if t == nil || model == "" {
		return Rate{}, false
	}
	m := normalizeModel(model)
	for _, e := range t.entries {
		if m == e.prefix || strings.HasPrefix(m, e.prefix+"-") || strings.HasPrefix(m, e.prefix+":") || strings.HasPrefix(m, e.prefix+".") {
			return e.rate, true
		}
	}
	// Secondary pass: loose prefix match (e.g., "claude-3-5-sonnet-20241022" or "gpt-4o-2024-08-06")
	for _, e := range t.entries {
		if strings.HasPrefix(m, e.prefix) {
			return e.rate, true
		}
	}
	return Rate{}, false
}

// Estimate calculates USD cost (rounded to 6 decimal places, clamped to [0, 10000]).
// Returns (cost, true) if the model matched a rate, or (0, false) if unknown.
func (t *Table) Estimate(model string, input, output, cacheRead, cacheWrite int64) (float64, bool) {
	rate, ok := t.Lookup(model)
	if !ok {
		return 0, false
	}
	cost := (float64(input)*rate.Input +
		float64(output)*rate.Output +
		float64(cacheRead)*rate.CacheRead +
		float64(cacheWrite)*rate.CacheWrite) / 1_000_000.0
	if math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
		return 0, true
	}
	cost = math.Round(cost*1e6) / 1e6
	if cost > 10_000 {
		cost = 10_000
	}
	return cost, true
}

func normalizeKey(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// normalizeModel strips common provider prefixes like "anthropic/", "openai/", "models/".
func normalizeModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	m = strings.TrimPrefix(m, "models/")
	if idx := strings.LastIndex(m, "/"); idx >= 0 && idx+1 < len(m) {
		m = m[idx+1:]
	}
	return m
}
