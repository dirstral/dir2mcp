package providerhttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

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
	m := paramRejectionPhrase.FindStringSubmatch(pErr.Message)
	return len(m) == 2 && strings.EqualFold(m[1], name)
}
