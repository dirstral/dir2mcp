package tests

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// TestValidateEmbedVectors pins the shared provider-output boundary check
// (issue #703). Every embed adapter and the embedding worker run their vectors
// through it, so this is the single place the "what is a usable embedding"
// contract is stated: non-empty, right dimension when one is known, finite, and
// with an actual direction.
func TestValidateEmbedVectors(t *testing.T) {
	cases := []struct {
		name    string
		wantDim int
		vectors [][]float32
		wantErr bool
	}{
		{"healthy", 0, [][]float32{{1, 0}, {0.1, -0.9}}, false},
		{"tiny but non-zero", 0, [][]float32{{0, 1e-8}}, false},
		{"negative components are fine", 0, [][]float32{{-1, -1}}, false},
		{"no vectors at all", 0, nil, false}, // an empty batch is not malformed
		{"zero norm", 0, [][]float32{{0, 0, 0}}, true},
		{"zero norm among healthy siblings", 0, [][]float32{{1, 0}, {0, 0}, {0, 1}}, true},
		{"negative zero is still zero", 0, [][]float32{{float32(math.Copysign(0, -1)), 0}}, true},
		{"empty vector", 0, [][]float32{{}}, true},
		{"NaN component", 0, [][]float32{{float32(math.NaN()), 1}}, true},
		{"+Inf component", 0, [][]float32{{float32(math.Inf(1)), 1}}, true},
		{"-Inf component", 0, [][]float32{{float32(math.Inf(-1)), 1}}, true},
		{"requested dimension honored", 3, [][]float32{{1, 0, 0}}, false},
		{"short of the requested dimension", 3, [][]float32{{1, 0}}, true},
		{"longer than the requested dimension", 3, [][]float32{{1, 0, 0, 0}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := model.ValidateEmbedVectors("TEST_FAILED", tc.wantDim, tc.vectors)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if err == nil {
				return
			}
			var pErr *model.ProviderError
			if !errors.As(err, &pErr) {
				t.Fatalf("error %v is not a *model.ProviderError", err)
			}
			// Malformed output is terminal: the same response comes back on
			// retry, so the chunk must be marked failed rather than retried
			// forever.
			if pErr.Retryable {
				t.Fatalf("malformed output must be non-retryable: %v", pErr)
			}
			if pErr.Code != "TEST_FAILED" {
				t.Fatalf("code = %q, want the caller's provider code", pErr.Code)
			}
		})
	}
}

// TestValidateEmbedVectors_MessageIsNotMisclassified guards a subtle downstream
// hazard: these error strings are keyword-classified by store.ClassifyError /
// IsTransientError. A message that happened to contain an HTTP status token
// ("503", "429", …) would be read as a TRANSIENT upstream failure, and the
// embedding worker would leave the chunk PENDING and retry it forever instead
// of recording a terminal failure. The message therefore carries no batch index.
func TestValidateEmbedVectors_MessageIsNotMisclassified(t *testing.T) {
	// A batch large enough that a per-item index would reach 503/529.
	vectors := make([][]float32, 600)
	for i := range vectors {
		vectors[i] = []float32{1, 0}
	}
	vectors[503] = []float32{0, 0}
	vectors[529] = []float32{0, 0}

	err := model.ValidateEmbedVectors("TEST_FAILED", 0, vectors)
	if err == nil {
		t.Fatal("want an error for the zero vectors")
	}
	for _, token := range []string{"503", "529", "429", "401", "403", "413"} {
		if strings.Contains(err.Error(), token) {
			t.Fatalf("error message %q contains %q, which downstream keyword classification reads as a transient/auth failure", err, token)
		}
	}
}
