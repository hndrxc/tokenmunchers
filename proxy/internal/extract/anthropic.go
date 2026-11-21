package extract

import "encoding/json"

type anthropicUsage struct {
	InputTokens              *int64               `json:"input_tokens"`
	OutputTokens             *int64               `json:"output_tokens"`
	CacheReadInputTokens     *int64               `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64               `json:"cache_creation_input_tokens"`
	Iterations               []anthropicIteration `json:"iterations"`
}

type anthropicIteration struct {
	Type                     string `json:"type"`
	Model                    string `json:"model"`
	InputTokens              int64  `json:"input_tokens"`
	OutputTokens             int64  `json:"output_tokens"`
	CacheReadInputTokens     int64  `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"`
}

type anthropicEnvelope struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Usage   *anthropicUsage `json:"usage"`
	Message *struct {
		ID    string          `json:"id"`
		Model string          `json:"model"`
		Usage *anthropicUsage `json:"usage"`
	} `json:"message"`
}

type anthropicAcc struct {
	hint RequestHint

	responseID string
	model      string

	sawNonStream bool
	sawMsgStart  bool
	sawMsgDelta  bool
	sawMsgStop   bool
	inputTokens  int64
	outputTokens int64
	cacheRead    int64
	cacheWrite   int64
	// iterations is the latest per-model aggregation of reported billing
	// iterations, bounded by maxUsageRecords.
	iterations  *iterationBook
	hasAnyUsage bool
}

// overflowModel receives usage for models beyond the per-response record cap so
// total billed tokens are preserved without unbounded growth.
const overflowModel = "unknown"

type iterationTotals struct {
	model      string
	input      int64
	output     int64
	cacheRead  int64
	cacheWrite int64
}

// iterationBook aggregates usage per model in first-seen order, holding at most
// maxUsageRecords entries (one reserved for overflowModel).
type iterationBook struct {
	list  []iterationTotals
	index map[string]int
}

func newIterationBook() *iterationBook {
	return &iterationBook{index: make(map[string]int)}
}

func (b *iterationBook) add(model string, in, out, cacheRead, cacheWrite int64) {
	model = SanitizeModel(model)
	i, ok := b.index[model]
	if !ok {
		if len(b.list) >= maxUsageRecords-1 {
			model = overflowModel
			i, ok = b.index[model]
		}
		if !ok {
			i = len(b.list)
			b.list = append(b.list, iterationTotals{model: model})
			b.index[model] = i
		}
	}
	e := &b.list[i]
	e.input += nonNeg(in)
	e.output += nonNeg(out)
	e.cacheRead += nonNeg(cacheRead)
	e.cacheWrite += nonNeg(cacheWrite)
}

func newAnthropicAcc(hint RequestHint) *anthropicAcc {
	return &anthropicAcc{hint: hint}
}

func (a *anthropicAcc) Feed(raw []byte) {
	var env anthropicEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return
	}

	switch env.Type {
	case "message_start":
		a.sawMsgStart = true
		if env.Message != nil {
			if env.Message.ID != "" {
				a.responseID = boundID(env.Message.ID)
			}
			if env.Message.Model != "" {
				a.model = env.Message.Model
			}
			a.mergeUsage(env.Message.Usage, true)
		}
	case "message_delta":
		a.sawMsgDelta = true
		a.mergeUsage(env.Usage, false)
	case "message_stop":
		a.sawMsgStop = true
	case "message":
		// Non-streaming Anthropic Messages API response.
		a.sawNonStream = true
		if env.ID != "" {
			a.responseID = boundID(env.ID)
		}
		if env.Model != "" {
			a.model = env.Model
		}
		a.mergeUsage(env.Usage, false)
	default:
		// Guard against non-streaming payloads or wrappers that omit "type":"message"
		// while carrying Anthropic-specific cache fields.
		if env.Usage != nil && (env.Usage.CacheReadInputTokens != nil || env.Usage.CacheCreationInputTokens != nil || len(env.Usage.Iterations) > 0) {
			a.sawNonStream = true
			if env.ID != "" {
				a.responseID = boundID(env.ID)
			}
			if env.Model != "" {
				a.model = env.Model
			}
			a.mergeUsage(env.Usage, false)
		}
	}
}

func (a *anthropicAcc) mergeUsage(u *anthropicUsage, isStartPlaceholder bool) {
	if u == nil {
		return
	}
	a.hasAnyUsage = true
	if u.InputTokens != nil {
		a.inputTokens = nonNeg(*u.InputTokens)
	}
	if u.OutputTokens != nil {
		// In message_start, output_tokens is typically a placeholder (e.g. 1).
		// We store it, and message_delta overwrites it; Finalize guards against
		// reporting an unverified placeholder when the stream was cut short.
		if !isStartPlaceholder || !a.sawMsgDelta {
			a.outputTokens = nonNeg(*u.OutputTokens)
		}
	}
	if u.CacheReadInputTokens != nil {
		a.cacheRead = nonNeg(*u.CacheReadInputTokens)
	}
	if u.CacheCreationInputTokens != nil {
		a.cacheWrite = nonNeg(*u.CacheCreationInputTokens)
	}
	if len(u.Iterations) > 0 {
		book := newIterationBook()
		for _, it := range u.Iterations {
			book.add(it.Model, it.InputTokens, it.OutputTokens, it.CacheReadInputTokens, it.CacheCreationInputTokens)
		}
		a.iterations = book
	}
}

func (a *anthropicAcc) Finalize(clientDisconnected bool) []UsageRecord {
	if !a.hasAnyUsage {
		return nil
	}
	baseModel := a.model
	if baseModel == "" {
		baseModel = a.hint.Model
	}

	// Handle multi-iteration responses (e.g., Anthropic compaction / fallback)
	// by aggregating per model so billed tokens across models are preserved accurately.
	if a.iterations != nil {
		// Iterations without a model belong to the response's base model.
		book := newIterationBook()
		for _, e := range a.iterations.list {
			m := e.model
			if m == "" {
				m = baseModel
			}
			book.add(m, e.input, e.output, e.cacheRead, e.cacheWrite)
		}
		out := make([]UsageRecord, 0, len(book.list))
		for _, e := range book.list {
			out = append(out, UsageRecord{
				ResponseID:       a.responseID,
				Model:            e.model,
				InputTokens:      e.input,
				OutputTokens:     e.output,
				CacheReadTokens:  e.cacheRead,
				CacheWriteTokens: e.cacheWrite,
			})
		}
		return out
	}

	output := a.outputTokens
	// If we only ever saw message_start (never message_delta or non-stream message)
	// because the stream was cut short, do not treat the start-of-stream placeholder as real output.
	if a.sawMsgStart && !a.sawMsgDelta && !a.sawNonStream && (clientDisconnected || !a.sawMsgStop) {
		output = 0
	}

	return []UsageRecord{{
		ResponseID:       a.responseID,
		Model:            baseModel,
		InputTokens:      a.inputTokens,
		OutputTokens:     output,
		CacheReadTokens:  a.cacheRead,
		CacheWriteTokens: a.cacheWrite,
	}}
}
