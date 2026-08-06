package tests

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/identity"
)

// The corpus id (SPEC §5.5) is what scopes a corpus's jobs inside a broker that
// several corpora may share (#708). These tests pin the three properties that
// makes it safe to put there: it distinguishes corpora, it is stable once
// written, and it discloses nothing about the corpus it names.

// memSettings is an in-memory settings table with the store's contract: a
// missing key reads back as os.ErrNotExist.
type memSettings struct {
	values  map[string]string
	readErr error
	writes  int
}

func newMemSettings() *memSettings { return &memSettings{values: map[string]string{}} }

func (m *memSettings) GetSetting(_ context.Context, key string) (string, error) {
	if m.readErr != nil {
		return "", m.readErr
	}
	v, ok := m.values[key]
	if !ok {
		return "", os.ErrNotExist
	}
	return v, nil
}

func (m *memSettings) SetSetting(_ context.Context, key, value string) error {
	m.writes++
	m.values[key] = value
	return nil
}

func TestCorpusID_DistinguishesCorpora(t *testing.T) {
	cases := []struct{ name, a, b string }{
		{"different local roots", identity.CorpusKey("/srv/alpha"), identity.CorpusKey("/srv/beta")},
		{"different buckets", identity.CorpusKeyForS3("alpha", "", ""), identity.CorpusKeyForS3("beta", "", "")},
		{"different prefixes of one bucket", identity.CorpusKeyForS3("shared", "alpha", ""), identity.CorpusKeyForS3("shared", "beta", "")},
		{"same bucket on different endpoints", identity.CorpusKeyForS3("c", "", "minio-a.example:9000"), identity.CorpusKeyForS3("c", "", "minio-b.example:9000")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if identity.CorpusID(tc.a) == identity.CorpusID(tc.b) {
				t.Fatalf("two corpora share one id; on a shared broker they would share a job namespace")
			}
		})
	}
}

// TestCorpusID_SpellingsOfOneCorpusAgree pins that the normalization #737
// established for the instance name governs the corpus id too, so one
// deployment cannot become two identities depending on how it was written down.
func TestCorpusID_SpellingsOfOneCorpusAgree(t *testing.T) {
	want := identity.CorpusID(identity.CorpusKeyForS3("bkt", "corpus", ""))
	for _, spelling := range []string{"corpus/", "/corpus", "/corpus/", "corpus//"} {
		if got := identity.CorpusID(identity.CorpusKeyForS3("bkt", spelling, "")); got != want {
			t.Fatalf("prefix spelling %q yields a different corpus id", spelling)
		}
	}
	if got := identity.CorpusID(identity.CorpusKeyForS3("BKT", "corpus", "")); got != want {
		t.Fatalf("bucket case changes the corpus id")
	}
	if got := identity.CorpusID(identity.CorpusKey("/srv/alpha/")); got != identity.CorpusID(identity.CorpusKey("/srv/alpha")) {
		t.Fatalf("a trailing slash changes the corpus id")
	}
}

// TestCorpusID_CarriesNoCorpusDetail is the property that lets the id sit in a
// queue several corpora can read: it must not describe the corpus. A path, a
// bucket, or an endpoint appearing in the id would publish one tenant's layout
// to another.
func TestCorpusID_CarriesNoCorpusDetail(t *testing.T) {
	id := identity.CorpusID(identity.CorpusKeyForS3("customer-a-private", "invoices/2026", "https://key:secret@minio.internal:9000"))
	for _, leak := range []string{"customer", "private", "invoices", "2026", "minio", "internal", "key", "secret", "s3"} {
		if strings.Contains(strings.ToLower(id), leak) {
			t.Fatalf("corpus id %q contains %q; the id reaches a shared broker and its logs", id, leak)
		}
	}
	if !strings.HasPrefix(id, "corpus-") {
		t.Fatalf("corpus id %q lost its prefix", id)
	}
	local := identity.CorpusID(identity.CorpusKey("/home/alice/Documents/taxes"))
	for _, leak := range []string{"alice", "documents", "taxes", "home"} {
		if strings.Contains(strings.ToLower(local), leak) {
			t.Fatalf("corpus id %q contains %q from the local root", local, leak)
		}
	}
}

// TestResolveCorpusID_SeedsThenIsStable is the reason the id is PERSISTED rather
// than derived on demand: a corpus that moves keeps the identity its queued
// jobs, its written vectors and its running workers are already bound to.
func TestResolveCorpusID_SeedsThenIsStable(t *testing.T) {
	ctx := context.Background()
	settings := newMemSettings()

	first, err := identity.ResolveCorpusID(ctx, settings, identity.CorpusKey("/srv/alpha"))
	if err != nil {
		t.Fatalf("ResolveCorpusID: %v", err)
	}
	if first != identity.CorpusID(identity.CorpusKey("/srv/alpha")) {
		t.Fatalf("seeded id %q is not the derived id", first)
	}
	if settings.values[identity.CorpusIDSettingKey] != first {
		t.Fatalf("corpus id was not persisted to settings.%s", identity.CorpusIDSettingKey)
	}

	// Same store, corpus now mounted somewhere else: the persisted value wins.
	moved, err := identity.ResolveCorpusID(ctx, settings, identity.CorpusKey("/mnt/relocated/alpha"))
	if err != nil {
		t.Fatalf("ResolveCorpusID after move: %v", err)
	}
	if moved != first {
		t.Fatalf("moving the corpus renamed it from %q to %q; every job already queued under the old id "+
			"would be orphaned", first, moved)
	}
	if settings.writes != 1 {
		t.Fatalf("settings written %d times, want 1: a resolved id must not be rewritten on every start", settings.writes)
	}
}

// TestResolveCorpusID_SurfacesReadFailures: a worker that cannot read the corpus
// identity must not guess one. Guessing is precisely the cross-wiring the id
// exists to prevent.
func TestResolveCorpusID_SurfacesReadFailures(t *testing.T) {
	settings := newMemSettings()
	settings.readErr = errors.New("database is locked")
	if _, err := identity.ResolveCorpusID(context.Background(), settings, identity.CorpusKey("/srv/alpha")); err == nil {
		t.Fatal("ResolveCorpusID silently derived an id after a settings read failure")
	}
}
