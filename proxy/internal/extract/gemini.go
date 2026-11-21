package extract

import "encoding/json"

type geminiEnvelope struct {
	ResponseID    string `json:"responseId"`
	ModelVersion  string `json:"modelVersion"`
	UsageMetadata *struct {
		PromptTokenCount        int64 `json:"promptTokenCount"`
		ToolUsePromptTokenCount int64 `json:"toolUsePromptTokenCount"`
		CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
		CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
		ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
	} `json:"usageMetadata"`
}

type geminiAcc struct {
	hint       RequestHint
	responseID string
	model      string
	hasUsage   bool
	prompt     int64
	toolPrompt int64
	cached     int64
	candidates int64
	thoughts   int64
}

func newGeminiAcc(hint RequestHint) *geminiAcc {
	return &geminiAcc{hint: hint}
}

func (a *geminiAcc) Feed(raw []byte) {
	var env geminiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return
	}
	if env.ResponseID != "" {
		a.responseID = boundID(env.ResponseID)
	}
	if env.ModelVersion != "" {
		a.model = env.ModelVersion
	}
	if env.UsageMetadata != nil {
		u := env.UsageMetadata
		if u.PromptTokenCount > 0 || u.ToolUsePromptTokenCount > 0 || u.CachedContentTokenCount > 0 || u.CandidatesTokenCount > 0 || u.ThoughtsTokenCount > 0 {
			a.hasUsage = true
			// In Gemini SSE streams, later chunks carry updated cumulative usageMetadata.
			a.prompt = nonNeg(u.PromptTokenCount)
			a.toolPrompt = nonNeg(u.ToolUsePromptTokenCount)
			a.cached = nonNeg(u.CachedContentTokenCount)
			a.candidates = nonNeg(u.CandidatesTokenCount)
			a.thoughts = nonNeg(u.ThoughtsTokenCount)
		}
	}
}

func (a *geminiAcc) Finalize() []UsageRecord {
	if !a.hasUsage {
		return nil
	}
	model := a.model
	if model == "" {
		model = a.hint.Model
	}
	exclusiveInput := nonNeg(a.prompt + a.toolPrompt - a.cached)
	output := nonNeg(a.candidates + a.thoughts)

	return []UsageRecord{{
		ResponseID:      a.responseID,
		Model:           model,
		InputTokens:     exclusiveInput,
		OutputTokens:    output,
		CacheReadTokens: a.cached,
	}}
}
