package usage

// OpenAIUsage mirrors the `usage` object returned by OpenAI-compatible APIs
// (OpenAI, Mistral, Cohere v2, many local servers). Providers embed it in
// their response structs and call ToUsage to feed a Sink. All fields are
// optional; absent fields decode to 0.
type OpenAIUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	// InputTokens/OutputTokens are the newer field names some APIs use.
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// Empty reports whether no token counts were present at all.
func (u OpenAIUsage) Empty() bool {
	return u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0 &&
		u.InputTokens == 0 && u.OutputTokens == 0
}

// ToUsage converts the wire shape into a Usage, normalizing the prompt/input
// and completion/output aliases. Reported is set true so the caller records it
// as "provider reported usage" even when the counts happen to be zero.
func (u OpenAIUsage) ToUsage() Usage {
	prompt := u.PromptTokens
	if prompt == 0 {
		prompt = u.InputTokens
	}
	completion := u.CompletionTokens
	if completion == 0 {
		completion = u.OutputTokens
	}
	return Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      u.TotalTokens,
		Reported:         true,
	}
}

// GeminiUsage mirrors the `usageMetadata` object returned by the Gemini
// generateContent API.
type GeminiUsage struct {
	PromptTokenCount     int64 `json:"promptTokenCount"`
	CandidatesTokenCount int64 `json:"candidatesTokenCount"`
	TotalTokenCount      int64 `json:"totalTokenCount"`
}

// Empty reports whether no token counts were present.
func (u GeminiUsage) Empty() bool {
	return u.PromptTokenCount == 0 && u.CandidatesTokenCount == 0 && u.TotalTokenCount == 0
}

// ToUsage converts Gemini's usageMetadata into a Usage.
func (u GeminiUsage) ToUsage() Usage {
	return Usage{
		PromptTokens:     u.PromptTokenCount,
		CompletionTokens: u.CandidatesTokenCount,
		TotalTokens:      u.TotalTokenCount,
		Reported:         true,
	}
}
