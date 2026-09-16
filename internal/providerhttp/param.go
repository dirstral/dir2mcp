package providerhttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/dirstral/dir2mcp/internal/model"
)

// paramRejectionPhrase matches a rejection bound DIRECTLY to the parameter that
// follows it, for servers that return no structured `param`. The name must be
// the thing being refused, not merely a word in the sentence; the caller
// compares the captured name with the parameter it sent.
var paramRejectionPhrase = regexp.MustCompile(
	`(?i)(?:unsupported|unrecognized|unknown|invalid|extra)[ _-]*(?:parameter|argument|field|input)?s?\s*[:\s]\s*['"]?([a-z_]+)\b`)

// errorParam pulls the refused parameter's name out of an error body when the
// provider states it: OpenAI and its compatible servers (Gemini's OpenAI
// endpoint included) put it at `error.param`; Mistral puts it at the top-level
// `param`. The adapters' HTTP error constructors keep the whole body in
// ProviderError.Message, so the field is still there. Returns "" when the body
// is not JSON or names no parameter.
func errorParam(msg string) string {
	var body struct {
		Param string `json:"param"`
		Error struct {
			Param string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(msg), &body); err != nil {
		return ""
	}
	if p := strings.TrimSpace(body.Error.Param); p != "" {
		return p
	}
	return strings.TrimSpace(body.Param)
}

// errorMessage pulls the human-readable message out of an error body:
// `error.message` for OpenAI-shaped bodies, top-level `message` for Mistral
// and Cohere. The phrase rule must run on the DECODED text: inside the raw JSON
// a quoted name is `\"temperature\"`, and the backslash sits between the
// optional quote and the name, so the regular expression never reaches it.
// Returns "" when the body is not JSON or carries no message.
func errorMessage(msg string) string {
	var body struct {
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(msg), &body); err != nil {
		return ""
	}
	if m := strings.TrimSpace(body.Error.Message); m != "" {
		return m
	}
	return strings.TrimSpace(body.Message)
}

// RefusedParams remembers, per model, that an endpoint refused a request
// parameter by name, so a client spends one probe per model rather than one
// per request. It is keyed by model because a refusal is a property of the
// model, not of the endpoint: a client whose DefaultChatModel later changes to
// a model that accepts the parameter must pin it again. The zero value is
// ready to use and safe for concurrent workers.
type RefusedParams struct {
	m sync.Map // model -> struct{}
}

// Refused reports whether a refusal was recorded for model.
func (r *RefusedParams) Refused(model string) bool {
	_, ok := r.m.Load(strings.TrimSpace(model))
	return ok
}

// Record remembers that model refused the parameter.
func (r *RefusedParams) Record(model string) {
	r.m.Store(strings.TrimSpace(model), struct{}{})
}

// RefusesParam reports whether err is a 400 in which the provider refuses the
// named request parameter ITSELF: the structured `param` names it when the body
// carries one, otherwise the rejection phrase must be followed by the name. Any
// other error, and any 400 that merely mentions the word, is not a refusal.
//
// The binding matters (dir2mcp #959): OpenAI's message for one rejected
// parameter names another as the remedy in the same sentence ("Unsupported
// parameter: 'temperature' is not supported with this model. Use
// 'max_completion_tokens' instead."), so a substring test would retry the wrong
// thing. It is shared by every chat adapter that pins a parameter it may have to
// drop (the completion cap spelling, the temperature), so one rule decides what
// a refusal is.
func RefusesParam(err error, name string) bool {
	var pErr *model.ProviderError
	if !errors.As(err, &pErr) || pErr.StatusCode != http.StatusBadRequest {
		return false
	}
	if param := errorParam(pErr.Message); param != "" {
		return strings.EqualFold(param, name)
	}
	text := errorMessage(pErr.Message)
	if text == "" {
		text = pErr.Message
	}
	m := paramRejectionPhrase.FindStringSubmatch(text)
	return len(m) == 2 && strings.EqualFold(m[1], name)
}
