package ai

// Usage is what a provider reported about the work it did for one call.
//
// It exists because the counts used to stop at a log line. The chat endpoint
// returns them on every response, the cost model in `internal/billing` is
// built to price them, and the column to hold them was already on the usage
// event — but the client decoded the numbers, printed them and returned only
// the message content, so every row landed with null tokens and the cost fell
// back to a flat per-credit figure. Returning them alongside the content is
// what closes that gap.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// Reported says whether the provider actually told us what the call cost.
//
// A zero Usage is not a free call: transcription bills by audio length and
// reports no tokens at all, and a provider may omit the block entirely. The
// distinction matters downstream, where "unknown" has to stay null rather than
// be written as a zero that would read as a genuine measurement.
func (u Usage) Reported() bool {
	return u.PromptTokens > 0 || u.CompletionTokens > 0 || u.TotalTokens > 0
}

// chatCompletion is the part of a chat response this codebase reads: the first
// choice's content, and what the call cost. All three chat callers decoded an
// identical anonymous struct before; keeping one here means a field added to
// the provider's usage block is picked up in one place rather than three.
type chatCompletion struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func (r chatCompletion) usage() Usage {
	return Usage{
		PromptTokens:     r.Usage.PromptTokens,
		CompletionTokens: r.Usage.CompletionTokens,
		TotalTokens:      r.Usage.TotalTokens,
	}
}
