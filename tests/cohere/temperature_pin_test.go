package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
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
			_, _ = w.Write([]byte(`{"message":"unused"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":{"content":[{"type":"text","text":"grounded answer"}]}}`))
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
	c := newClient(rec.server(t).URL)
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
