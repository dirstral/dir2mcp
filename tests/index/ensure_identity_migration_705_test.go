package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/provider"
)

// TestEnsureIdentity_MigratedRecordingKeepsVectors pins the migration half of
// issues #705/#702 where it actually bites: EnsureIdentity is what WIPES an
// index, and it must use the same compatibility rule as the startup check.
//
// config.VerifyEmbedIdentity migrates a legacy recording (the SPEC 8.1.4
// field-count ladder, and the #705 blank-model grace) and lets startup proceed.
// If EnsureIdentity then byte-compares, it answers "different" for exactly those
// migrated corpora and Resets — a full, unannounced re-embed of a corpus the
// operator was just told was compatible. The two MUST agree.
func TestEnsureIdentity_MigratedRecordingKeepsVectors(t *testing.T) {
	ctx := context.Background()
	current := provider.EmbedIdentity(
		provider.Profile{Name: "openai", Kind: provider.KindOpenAI}, false, provider.EmbedContextualOff)

	// Each of these is a form a shipped build actually recorded for that same
	// profile, before a component existed or before models were resolved.
	recordings := map[string]string{
		"pre-#705 blank models":     "openai||||0|0|off|off|off",
		"pre-contextual (8 fields)": "openai||||0|0|off|off",
		"pre-base_url (7 fields)":   "openai|||0|0|off|off",
		"pre-late-chunking (6)":     "openai|||0|0|off",
		"pre-multimodal (5 fields)": "openai|||0|0",
		"pre-dimension (3 fields)":  "openai||",
	}
	for name, recorded := range recordings {
		t.Run(name, func(t *testing.T) {
			// Sanity: the startup check accepts this recording…
			if err := provider.VerifyEmbedIdentity(recorded, current); err != nil {
				t.Fatalf("VerifyEmbedIdentity rejected %q: %v", recorded, err)
			}
			idx := index.NewHNSWIndex("")
			if err := idx.Reset(ctx, recorded); err != nil {
				t.Fatalf("seed recorded identity: %v", err)
			}
			mustUpsert(t, idx, model.IndexPayload{ChunkID: 1, RelPath: "a.md"}, []float32{1, 0})

			// …so the index must NOT be wiped.
			if err := index.EnsureIdentity(ctx, idx, current); err != nil {
				t.Fatalf("EnsureIdentity: %v", err)
			}
			hits, err := idx.Search(ctx, []float32{1, 0}, 10, model.Filter{})
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if len(hits) != 1 {
				t.Fatalf("EnsureIdentity silently re-embedded a compatible corpus (recorded %q): vectors = %v",
					recorded, chunkIDs(hits))
			}
		})
	}
}

// TestEnsureIdentity_RealChangeStillResets is the other direction: a genuine
// vector-space change must still discard the vectors. The migration must not
// become a blanket "everything matches", or the corpus-lifetime invariant is
// gone.
func TestEnsureIdentity_RealChangeStillResets(t *testing.T) {
	ctx := context.Background()
	current := provider.EmbedIdentity(
		provider.Profile{Name: "openai", Kind: provider.KindOpenAI}, false, provider.EmbedContextualOff)

	for _, recorded := range []string{
		"cohere||||0|0|off|off|off",                          // different provider
		"openai||text-embedding-3-large||0|0|off|off|off",    // different concrete model
		"openai|https://proxy.internal/v1|||0|0|off|off|off", // different endpoint (#702 class)
		"openai||||512|0|off|off|off",                        // different requested dimension
	} {
		idx := index.NewHNSWIndex("")
		if err := idx.Reset(ctx, recorded); err != nil {
			t.Fatalf("seed: %v", err)
		}
		mustUpsert(t, idx, model.IndexPayload{ChunkID: 1, RelPath: "a.md"}, []float32{1, 0})
		if err := index.EnsureIdentity(ctx, idx, current); err != nil {
			t.Fatalf("EnsureIdentity: %v", err)
		}
		hits, _ := idx.Search(ctx, []float32{1, 0}, 10, model.Filter{})
		if len(hits) != 0 {
			t.Fatalf("recorded %q vs current %q: vectors from another space were kept: %v",
				recorded, current, chunkIDs(hits))
		}
		if id, _ := idx.Identity(ctx); id != current {
			t.Fatalf("identity after reset = %q, want %q", id, current)
		}
	}
}

// TestEnsureIdentity_FreshIndexStillRecords guards the case the compatibility
// rule must NOT absorb: an empty recorded identity is a fresh index, and Reset
// is what RECORDS the identity there. Treating "" as "compatible" would leave
// the index permanently unlabeled.
func TestEnsureIdentity_FreshIndexStillRecords(t *testing.T) {
	ctx := context.Background()
	idx := index.NewHNSWIndex("")
	current := provider.EmbedIdentity(
		provider.Profile{Name: "openai", Kind: provider.KindOpenAI}, false, provider.EmbedContextualOff)
	if err := index.EnsureIdentity(ctx, idx, current); err != nil {
		t.Fatalf("EnsureIdentity: %v", err)
	}
	if id, _ := idx.Identity(ctx); id != current {
		t.Fatalf("fresh index identity = %q, want %q", id, current)
	}
}
