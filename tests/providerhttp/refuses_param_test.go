package tests

import (
	"errors"
	"net/http"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/providerhttp"
)

// RefusesParam is the one rule every chat adapter uses to decide that a 400
// refused a parameter it sent, and so may retry once without it. It must bind
// to the parameter, not to a word in the sentence (#959), and it must read the
// structured name wherever a provider puts it.
func TestRefusesParam(t *testing.T) {
	bad := func(body string) error {
		return &model.ProviderError{Code: "X", Message: body, StatusCode: http.StatusBadRequest}
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"OpenAI-shaped error.param", bad(`{"error":{"message":"Unsupported value: 'temperature' does not support 0.","param":"temperature"}}`), true},
		// The prose here binds to nothing, so only the structured field can say
		// which parameter was refused; a detector that ignored the top-level
		// `param` would miss a Mistral refusal.
		{"Mistral-shaped top-level param, prose unbound", bad(`{"object":"error","message":"Input should be less than or equal to 1.5","type":"invalid_request_error","param":"temperature","code":"1000"}`), true},
		{"prose bound to the name", bad(`{"error":{"message":"Unsupported parameter: temperature is not supported when thinking is enabled."}}`), true},
		{"structured param names another parameter, ours only as remedy", bad(`{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported. Use 'temperature' instead.","param":"max_tokens"}}`), false},
		{"prose that merely mentions the word", bad(`{"error":{"message":"invalid request: the prompt is too long; the temperature setting was accepted."}}`), false},
		{"not a 400", &model.ProviderError{Code: "X", Message: `{"error":{"param":"temperature"}}`, StatusCode: http.StatusInternalServerError}, false},
		{"not a ProviderError", errors.New(`{"error":{"param":"temperature"}}`), false},
	} {
		if got := providerhttp.RefusesParam(tc.err, "temperature"); got != tc.want {
			t.Errorf("%s: RefusesParam = %v, want %v", tc.name, got, tc.want)
		}
	}
}
