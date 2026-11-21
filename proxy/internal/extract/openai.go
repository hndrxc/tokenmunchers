package extract

import (
	"encoding/json"
	"math"
)

type openaiUsage struct {
	PromptTokens          *int64   `json:"prompt_tokens"`
	CompletionTokens      *int64   `json:"completion_tokens"`
	TotalTokens           *int64   `json:"total_tokens"`
	InputTokens           *int64   `json:"input_tokens"`
	OutputTokens          *int64   `json:"output_tokens"`
	CacheCreationTokens   *int64   `json:"cache_creation_input_tokens"`
	CacheWriteInputTokens *int64   `json:"cache_write_input_tokens"`
	Cost                  *float64 `json:"cost"`
	CostInUSDTicks        *int64   `json:"cost_in_usd_ticks"`

	PromptTokensDetails *struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails *struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

type openaiEnvelope struct {
	Type     string       `json:"type"`
	ID       string       `json:"id"`
	Model    string       `json:"model"`
	Usage    *openaiUsage `json:"usage"`
	Response *struct {
		ID    string       `json:"id"`
		Model string       `json:"model"`
		Usage *openaiUsage `json:"usage"`
	} `json:"response"`
}

type openaiAcc struct {
	hint       RequestHint
	responseID string
	model      string
	usage      *openaiUsage
}

func newOpenAIAcc(hint RequestHint) *openaiAcc {
	return &openaiAcc{hint: hint}
}

func (a *openaiAcc) Feed(raw []byte) {
	var env openaiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return
	}

	// OpenAI Responses API & ChatGPT/Codex streaming envelope:
	// {"type": "response.completed", "response": {"id": "...", "model": "...", "usage": {...}}}
	if env.Response != nil {
		if env.Response.ID != "" {
			a.responseID = boundID(env.Response.ID)
		}
		if env.Response.Model != "" {
			a.model = env.Response.Model
		}
		if env.Response.Usage != nil && hasOpenAIUsage(env.Response.Usage) {
			a.usage = env.Response.Usage
		}
		return
	}

	// Ignore Anthropic message envelopes when FormatAuto runs both accumulators.
	if env.Type == "message" || env.Type == "message_start" || env.Type == "message_delta" {
		return
	}

	if env.ID != "" && a.responseID == "" {
		a.responseID = boundID(env.ID)
	}
	if env.Model != "" {
		a.model = env.Model
	}
	if env.Usage != nil && hasOpenAIUsage(env.Usage) {
		a.usage = env.Usage
	}
}

func hasOpenAIUsage(u *openaiUsage) bool {
	return u.PromptTokens != nil || u.CompletionTokens != nil || u.InputTokens != nil || u.OutputTokens != nil
}

func (a *openaiAcc) Finalize() []UsageRecord {
	if a.usage == nil {
		return nil
	}
	u := a.usage

	var promptTotal int64
	if u.PromptTokens != nil {
		promptTotal = nonNeg(*u.PromptTokens)
	} else if u.InputTokens != nil {
		promptTotal = nonNeg(*u.InputTokens)
	}

	var completion int64
	if u.CompletionTokens != nil {
		completion = nonNeg(*u.CompletionTokens)
	} else if u.OutputTokens != nil {
		completion = nonNeg(*u.OutputTokens)
	}

	var cacheRead int64
	var cacheWrite int64
	if u.PromptTokensDetails != nil {
		cacheRead = nonNeg(u.PromptTokensDetails.CachedTokens)
		cacheWrite = nonNeg(u.PromptTokensDetails.CacheWriteTokens)
	}
	if u.InputTokensDetails != nil {
		if u.InputTokensDetails.CachedTokens > cacheRead {
			cacheRead = nonNeg(u.InputTokensDetails.CachedTokens)
		}
		if u.InputTokensDetails.CacheWriteTokens > cacheWrite {
			cacheWrite = nonNeg(u.InputTokensDetails.CacheWriteTokens)
		}
	}
	if u.CacheCreationTokens != nil && *u.CacheCreationTokens > cacheWrite {
		cacheWrite = nonNeg(*u.CacheCreationTokens)
	}
	if u.CacheWriteInputTokens != nil && *u.CacheWriteInputTokens > cacheWrite {
		cacheWrite = nonNeg(*u.CacheWriteInputTokens)
	}

	// OpenAI/Responses/OpenRouter/xAI prompt_tokens/input_tokens include cached tokens;
	// TokenMunchers stores disjoint categories where total = input + output + cache_read + cache_write.
	exclusiveInput := nonNeg(promptTotal - cacheRead - cacheWrite)

	var reasoning int64
	if u.CompletionTokensDetails != nil {
		reasoning = nonNeg(u.CompletionTokensDetails.ReasoningTokens)
	} else if u.OutputTokensDetails != nil {
		reasoning = nonNeg(u.OutputTokensDetails.ReasoningTokens)
	}

	output := completion
	// xAI/Grok sometimes reports reasoning_tokens outside completion_tokens, which is
	// observable when total_tokens >= prompt + completion + reasoning.
	if reasoning > 0 && u.TotalTokens != nil && *u.TotalTokens >= promptTotal+completion+reasoning {
		output = completion + reasoning
	}

	var reportedCost *float64
	if u.Cost != nil && !math.IsNaN(*u.Cost) && !math.IsInf(*u.Cost, 0) && *u.Cost >= 0 {
		c := *u.Cost
		reportedCost = &c
	} else if u.CostInUSDTicks != nil && *u.CostInUSDTicks > 0 {
		c := float64(*u.CostInUSDTicks) / 1e10
		reportedCost = &c
	}

	model := a.model
	if model == "" {
		model = a.hint.Model
	}

	return []UsageRecord{{
		ResponseID:       a.responseID,
		Model:            model,
		InputTokens:      exclusiveInput,
		OutputTokens:     output,
		CacheReadTokens:  cacheRead,
		CacheWriteTokens: cacheWrite,
		ReportedCostUSD:  reportedCost,
	}}
}
