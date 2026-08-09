package cli

import (
	"strings"
	"testing"
)

// TestParseReindexOptions covers the flag contract of the #783 embed-step
// retry, including the combinations that must be refused rather than silently
// reinterpreted.
func TestParseReindexOptions(t *testing.T) {
	cases := []struct {
		name           string
		args           []string
		wantErr        string
		wantEmbedOnly  bool
		wantCategories []string
		wantPositional []string
	}{
		{
			name: "bare reindex keeps the full-rebuild shape",
			args: nil,
		},
		{
			name:          "embeddings-only",
			args:          []string{"--embeddings-only"},
			wantEmbedOnly: true,
		},
		{
			name:           "category list is split and lower-cased",
			args:           []string{"--embeddings-only", "--error-category", "Auth, rate_limit"},
			wantEmbedOnly:  true,
			wantCategories: []string{"auth", "rate_limit"},
		},
		{
			name:           "positional args survive for the caller to reject",
			args:           []string{"extra"},
			wantPositional: []string{"extra"},
		},
		{
			// A full rebuild re-creates every chunk, so a per-category filter has
			// no meaning there; accepting it would imply a scoped run that is in
			// fact a whole-corpus reprocess.
			name:    "category filter requires embeddings-only",
			args:    []string{"--error-category", "auth"},
			wantErr: "only valid with --embeddings-only",
		},
		{
			name:    "unknown category is rejected with the vocabulary",
			args:    []string{"--embeddings-only", "--error-category", "auth,nope"},
			wantErr: "unknown error category",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, positional, err := parseReindexOptions(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseReindexOptions: %v", err)
			}
			if opts.embeddingsOnly != tc.wantEmbedOnly {
				t.Errorf("embeddingsOnly = %v, want %v", opts.embeddingsOnly, tc.wantEmbedOnly)
			}
			if strings.Join(opts.errorCategories, ",") != strings.Join(tc.wantCategories, ",") {
				t.Errorf("errorCategories = %v, want %v", opts.errorCategories, tc.wantCategories)
			}
			if strings.Join(positional, ",") != strings.Join(tc.wantPositional, ",") {
				t.Errorf("positional = %v, want %v", positional, tc.wantPositional)
			}
		})
	}
}

// TestSplitFailedCounts pins that the report separates what the run retried
// from what it deliberately left in place, so a partial retry never reads as a
// complete one.
func TestSplitFailedCounts(t *testing.T) {
	failed := map[string]int64{"auth": 346, "payload_too_large": 2, "quality_gate": 5}
	retried, terminal := splitFailedCounts(failed, []string{"auth", "rate_limit"})

	if retried["auth"] != 346 || len(retried) != 1 {
		t.Errorf("retried = %v, want only auth=346", retried)
	}
	if terminal["payload_too_large"] != 2 || terminal["quality_gate"] != 5 || len(terminal) != 2 {
		t.Errorf("terminal = %v, want payload_too_large=2 quality_gate=5", terminal)
	}
	if got := formatCategoryCounts(terminal); got != "payload_too_large=2 quality_gate=5" {
		t.Errorf("formatCategoryCounts = %q", got)
	}
	if got := formatCategoryCounts(nil); got != "" {
		t.Errorf("formatCategoryCounts(nil) = %q, want empty", got)
	}
}
