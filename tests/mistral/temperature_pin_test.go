package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/mistral"
)

// A derived representation must be reproducible, whichever chat provider
// produced it (dir2mcp #996 pinned the OpenAI adapter; this adapter follows).
// These tests pin the wire field, because the wire is where it matters.

// tempRecorder is a fake chat endpoint that records whether each request
// carried `temperature` and with what value, and can refuse it the way this
// provider does.
type tempRecorder struct {
	mu       sync.Mutex
	carried  []bool
	values   []float64
	refuse   bool
	refusals int
	other400 string // when set, every request fails 400 with this body
}

func (r *tempRecorder) server(t *testing.T) *httptest.Server {
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
		other := r.other400
		r.mu.Unlock()
		switch {
		case other != "":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(other))
			return
		case refuse:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"object":"error","message":"Invalid parameter: temperature","type":"invalid_request_error","param":"temperature","code":"1000"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"grounded answer"}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (r *tempRecorder) snapshot() ([]bool, []float64, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.carried...), append([]float64(nil), r.values...), r.refusals
}

// A fresh client pins temperature 0 on the first request, with no probe.
func TestGenerate_PinsTemperatureZero(t *testing.T) {
	rec := &tempRecorder{}
	c := mistral.NewClient(rec.server(t).URL, "test-key")
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

// An endpoint that refuses the parameter by name gets one retry without it,
// and the refusal is remembered for the client's life.
func TestGenerate_DropsTemperatureWhenRefused(t *testing.T) {
	rec := &tempRecorder{refuse: true}
	c := mistral.NewClient(rec.server(t).URL, "test-key")
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

// A 400 that merely mentions the word is not a refusal. It surfaces after one
// request and does not drop the pin for later requests.
func TestGenerate_UnrelatedBadRequestKeepsThePin(t *testing.T) {
	rec := &tempRecorder{other400: `{"error":{"message":"invalid request: the prompt is too long for this model; the temperature setting was accepted."}}`}
	c := mistral.NewClient(rec.server(t).URL, "test-key")
	if _, err := c.Generate(context.Background(), "q"); err == nil {
		t.Fatal("an unrelated 400 must surface")
	}
	carried, _, _ := rec.snapshot()
	if len(carried) != 1 {
		t.Fatalf("server calls = %d, want 1: an unrelated 400 must not be retried without temperature", len(carried))
	}
	rec.mu.Lock()
	rec.other400 = ""
	rec.mu.Unlock()
	if _, err := c.Generate(context.Background(), "q2"); err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	carried, _, _ = rec.snapshot()
	if len(carried) != 2 || !carried[1] {
		t.Fatalf("an unrelated 400 must not drop the pin for later requests, got %v", carried)
	}
}
