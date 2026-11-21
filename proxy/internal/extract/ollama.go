package extract

import "encoding/json"

type ollamaEnvelope struct {
	Model                 string `json:"model"`
	Done                  bool   `json:"done"`
	PromptEvalCount       *int64 `json:"prompt_eval_count"`
	EvalCount             *int64 `json:"eval_count"`
	PromptEvalCachedCount *int64 `json:"prompt_eval_cached_count"`
}

type ollamaAcc struct {
	hint       RequestHint
	model      string
	hasUsage   bool
	promptEval int64
	evalCount  int64
	cached     int64
}

func newOllamaAcc(hint RequestHint) *ollamaAcc {
	return &ollamaAcc{hint: hint}
}

func (a *ollamaAcc) Feed(raw []byte) {
	var env ollamaEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return
	}
	if env.Model != "" {
		a.model = env.Model
	}
	if env.PromptEvalCount != nil || env.EvalCount != nil {
		a.hasUsage = true
		if env.PromptEvalCount != nil {
			a.promptEval = nonNeg(*env.PromptEvalCount)
		}
		if env.EvalCount != nil {
			a.evalCount = nonNeg(*env.EvalCount)
		}
		if env.PromptEvalCachedCount != nil {
			a.cached = nonNeg(*env.PromptEvalCachedCount)
		}
	}
}

func (a *ollamaAcc) Finalize() []UsageRecord {
	if !a.hasUsage {
		return nil
	}
	model := a.model
	if model == "" {
		model = a.hint.Model
	}
	zero := 0.0
	return []UsageRecord{{
		Model:           model,
		InputTokens:     nonNeg(a.promptEval - a.cached),
		OutputTokens:    a.evalCount,
		CacheReadTokens: a.cached,
		ReportedCostUSD: &zero,
	}}
}
