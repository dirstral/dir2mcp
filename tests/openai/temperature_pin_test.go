package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/openai"
)

// A derived representation must be reproducible: the same transcript translated
// twice must come back the same. Left to the server, a local OpenAI-compatible
// endpoint samples at its own default, and two runs of an unmodified pilot
// archive disagreed on 96.7% of subtitle cues. The client therefore pins
// temperature 0, and drops it only for an endpoint that refuses the parameter by
// name. These tests pin the wire field, because the wire is where it matters.

// temperatureRecorder is a fake /chat/completions that records whether each
// request carried `temperature` and with what value, and can refuse it the way
// a real server does: with a structured `error.param`, or with prose only.
type temperatureRecorder struct {
	mu         sync.Mutex
	carried    []bool
	values     []float64
	refuse     bool // refuse any request that carries temperature
	structured bool // refuse with error.param set (else prose only)
	refusals   int
	extraBody  string // when set, every request fails 400 with this body instead
}

func (r *temperatureRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		v, has := body["temperature"]
		r.mu.Lock()
		r.carried = append(r.carried, has)
		if f, ok := v.(float64); ok {
			r.values = append(r.values, f)
		}
		refuse := r.refuse && has
		if refuse {
			r.refusals++
		}
		structured, extra := r.structured, r.extraBody
		r.mu.Unlock()

		switch {
		case extra != "":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(extra))
			return
		case refuse && structured:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported value: 'temperature' does not support 0 with this model. Only the default (1) value is supported.","type":"invalid_request_error","param":"temperature","code":"unsupported_value"}}`))
			return
		case refuse:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported parameter: 'temperature' is not supported with this model."}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"grounded answer"}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (r *temperatureRecorder) snapshot() ([]bool, []float64, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.carried...), append([]float64(nil), r.values...), r.refusals
}

// A fresh client pins temperature 0 on the first request, with no probe.
func TestGenerate_PinsTemperatureZero(t *testing.T) {
	rec := &temperatureRecorder{}
	c := openai.NewClient(rec.server(t).URL+"/v1", "k")
	got, err := c.Generate(context.Background(), "translate this")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got != "grounded answer" {
		t.Fatalf("answer = %q", got)
	}
	carried, values, _ := rec.snapshot()
	if len(carried) != 1 || !carried[0] {
		t.Fatalf("the request must carry temperature, got carried=%v", carried)
	}
	if len(values) != 1 || values[0] != 0 {
		t.Fatalf("temperature = %v, want exactly 0: any sampling makes two runs of one corpus disagree", values)
	}
}

// An endpoint that refuses the parameter by name (structured `param`) gets one
// retry without it, and the refusal is remembered for the client's life.
func TestGenerate_DropsTemperatureWhenRefused_Structured(t *testing.T) {
	rec := &temperatureRecorder{refuse: true, structured: true}
	c := openai.NewClient(rec.server(t).URL+"/v1", "k")
	if _, err := c.Generate(context.Background(), "q"); err != nil {
		t.Fatalf("a model that refuses temperature must still be usable: %v", err)
	}
	carried, _, refusals := rec.snapshot()
	if len(carried) != 2 || !carried[0] || carried[1] {
		t.Fatalf("wire sequence = %v, want [with-temperature without-temperature]", carried)
	}
	if refusals != 1 {
		t.Fatalf("refusals = %d, want 1", refusals)
	}
	if _, err := c.Generate(context.Background(), "again"); err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	carried, _, refusals = rec.snapshot()
	if len(carried) != 3 || carried[2] {
		t.Fatalf("after a refusal the client must keep omitting temperature, got %v", carried)
	}
	if refusals != 1 {
		t.Fatalf("refusals = %d, want still 1: the refusal must be remembered, not re-probed", refusals)
	}
}

// A server that returns prose only is handled when the rejection is bound to
// the parameter name.
func TestGenerate_DropsTemperatureWhenRefused_ProseOnly(t *testing.T) {
	rec := &temperatureRecorder{refuse: true}
	c := openai.NewClient(rec.server(t).URL+"/v1", "k")
	if _, err := c.Generate(context.Background(), "q"); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	carried, _, _ := rec.snapshot()
	if len(carried) != 2 || !carried[0] || carried[1] {
		t.Fatalf("wire sequence = %v, want [with-temperature without-temperature]", carried)
	}
}

// A 400 that merely mentions the word is not a refusal of the parameter. It
// must surface after one request, and it must not flip the client: a later
// request still carries the pin.
func TestGenerate_UnrelatedBadRequestKeepsThePin(t *testing.T) {
	rec := &temperatureRecorder{extraBody: `{"error":{"message":"invalid request: the prompt is too long for this model; the temperature setting was accepted."}}`}
	c := openai.NewClient(rec.server(t).URL+"/v1", "k")
	_, err := c.Generate(context.Background(), "q")
	if err == nil {
		t.Fatal("an unrelated 400 must surface")
	}
	if !strings.Contains(err.Error(), "too long") {
		t.Fatalf("error must name the real cause, got %v", err)
	}
	carried, _, _ := rec.snapshot()
	if len(carried) != 1 {
		t.Fatalf("server calls = %d, want 1: an unrelated 400 must not be retried without temperature", len(carried))
	}
	rec.mu.Lock()
	rec.extraBody = ""
	rec.mu.Unlock()
	if _, err := c.Generate(context.Background(), "q2"); err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	carried, _, _ = rec.snapshot()
	if len(carried) != 2 || !carried[1] {
		t.Fatalf("an unrelated 400 must not drop the pin for later requests, got %v", carried)
	}
}

// An endpoint that refuses BOTH the modern cap name and the temperature
// parameter still gets an answer: each refusal costs one probe, once.
func TestGenerate_CapAndTemperatureFallbacksCompose(t *testing.T) {
	var mu sync.Mutex
	var seen []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		mu.Lock()
		seen = append(seen, body)
		mu.Unlock()
		if _, ok := body["max_completion_tokens"]; ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported parameter: 'max_completion_tokens' is not supported. Use 'max_tokens' instead.","param":"max_completion_tokens"}}`))
			return
		}
		if _, ok := body["temperature"]; ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported parameter: 'temperature' is not supported with this model.","param":"temperature"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"grounded answer"}}]}`))
	}))
	t.Cleanup(srv.Close)
	c := openai.NewClient(srv.URL+"/v1", "k")
	got, err := c.Generate(context.Background(), "q")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got != "grounded answer" {
		t.Fatalf("answer = %q", got)
	}
	mu.Lock()
	n := len(seen)
	last := seen[n-1]
	mu.Unlock()
	if n != 3 {
		t.Fatalf("server calls = %d, want 3 (modern cap, legacy cap with temperature, legacy cap without)", n)
	}
	if _, ok := last["temperature"]; ok {
		t.Fatalf("the final request must omit temperature: %v", last)
	}
	if _, ok := last["max_tokens"]; !ok {
		t.Fatalf("the final request must carry the legacy cap: %v", last)
	}
	if _, err := c.Generate(context.Background(), "q2"); err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	mu.Lock()
	n2 := len(seen)
	mu.Unlock()
	if n2 != 4 {
		t.Fatalf("both refusals must be remembered: server calls = %d, want 4", n2)
	}
}

// OpenAI's own refusal of temperature names the cap as an unrelated remedy in
// the same sentence. That must drop temperature and must NOT flip the cap
// spelling (#959): the client stays on max_completion_tokens.
func TestGenerate_TemperatureRefusalNamingTheCapDoesNotFlipTheCap(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(req.Body).Decode(&b)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		if _, ok := b["temperature"]; ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported parameter: 'temperature' is not supported with this model. Use 'max_completion_tokens' instead.","type":"invalid_request_error","param":"temperature","code":"unsupported_parameter"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"grounded answer"}}]}`))
	}))
	t.Cleanup(srv.Close)
	c := openai.NewClient(srv.URL+"/v1", "k")
	if _, err := c.Generate(context.Background(), "q"); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("requests = %d, want 2 (with temperature, then without)", len(bodies))
	}
	for i, b := range bodies {
		if _, ok := b["max_completion_tokens"]; !ok {
			t.Fatalf("request %d left the modern cap name over a temperature refusal: %v", i+1, b)
		}
	}
	if _, ok := bodies[1]["temperature"]; ok {
		t.Fatalf("the retry must omit temperature: %v", bodies[1])
	}
}
