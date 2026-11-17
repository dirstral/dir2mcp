package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/internal/subexport"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// TestSecretPolicy_TwoPhaseRechecksExistingTranslations pins SPEC §7.2 (spec
// 0.96.0) under the two-phase split (§8.6.11). After a broader policy change,
// the transcription pass re-reads a media document and stamps its content hash,
// which clears the pending mark. The derivation pass keeps a translation that
// already exists under the current derivation identity, so without a fix an
// old translation that the new pattern matches stays searchable. The new
// pattern here matches only the translation text (codenameTranslator writes a
// codename that the source transcript does not hold), so only a check of the
// translation can withhold the document.
func TestSecretPolicy_TwoPhaseRechecksExistingTranslations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	mustWriteFile(t, filepath.Join(root, "media", "clip.mp3"), []byte("fake-audio"))
	stateDir := testutil.TempDir(t)
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const rel = "media/clip.mp3"

	run := func(patterns []string) {
		t.Helper()
		cfg := config.Default()
		cfg.RootDir = root
		cfg.StateDir = stateDir
		cfg.STTProvider = "off"
		cfg.MediaBatchTwoPhase = true
		cfg.SecretPatterns = patterns
		svc := mustNewIngestService(t, cfg, st)
		svc.SetTranscriber(&fakeTranscriber{text: "[00:00] the report covers the regional budget and the new bridge"})
		svc.SetSTTIdentity("whisper", "large-v3")
		svc.SetTranscriptLanguage("de")
		svc.SetTranslator(&codenameTranslator{}, "mistral", "model-a", []string{"en"})
		if err := svc.Run(ctx); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	liveTranslation := func() bool {
		t.Helper()
		reps, err := st.TranscriptRepresentations(ctx, rel)
		if err != nil {
			return false
		}
		for _, rep := range reps {
			if subexport.RepIsTranslation(rep.MetaJSON) {
				return true
			}
		}
		return false
	}

	defaults := config.Default().SecretPatterns
	run(defaults)
	if d, err := st.GetDocumentByPath(ctx, rel); err != nil || d.Status != string(model.DocumentStatusOK) {
		t.Fatalf("after the first run: status=%q err_msg=%q err=%v", d.Status, d.ErrorMessage, err)
	}
	if !liveTranslation() {
		t.Fatal("the first run produced no translation; the test proves nothing")
	}

	broader := append(append([]string(nil), defaults...), `BLUEHERON-[a-z]+-[0-9]{4}`)
	run(broader)
	doc := requireStatus(t, st, rel, model.DocumentStatusSecretExcluded)
	if doc.SkipReason != model.SkipReasonSecretExcluded {
		t.Errorf("skip_reason = %q, want %q", doc.SkipReason, model.SkipReasonSecretExcluded)
	}
	requireWithheldAndRetired(t, st, rel)
}

// codenameTranslator is fakeTranslator with one word of its output replaced by
// a codename, so the translation holds text that the source does not.
type codenameTranslator struct {
	fakeTranslator
}

func (c *codenameTranslator) Generate(ctx context.Context, prompt string) (string, error) {
	out, err := c.fakeTranslator.Generate(ctx, prompt)
	return strings.ReplaceAll(out, "bridge", "BLUEHERON-alpha-2026"), err
}

// recordingSummarizer keeps every prompt it was sent.
type recordingSummarizer struct {
	mu      sync.Mutex
	prompts []string
}

func (r *recordingSummarizer) Generate(_ context.Context, prompt string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prompts = append(r.prompts, prompt)
	return "a short summary of the report", nil
}

// TestSecretPolicy_SummaryNeverReadsUncheckedTranslations covers the summary
// step of the two-phase transcription pass (SPEC §7.2, §9.7). After a broader
// policy change, the existing translation is not checked yet. The summary step
// must not read it and must not send it to the summarizer: the translation is
// retired first, and the summary source is checked against the active policy.
// Without these steps, the third run below sends the translation to the
// summarizer.
func TestSecretPolicy_SummaryNeverReadsUncheckedTranslations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	mustWriteFile(t, filepath.Join(root, "media", "clip.mp3"), []byte("fake-audio"))
	stateDir := testutil.TempDir(t)
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const rel = "media/clip.mp3"

	run := func(patterns []string) *recordingSummarizer {
		t.Helper()
		cfg := config.Default()
		cfg.RootDir = root
		cfg.StateDir = stateDir
		cfg.STTProvider = "off"
		cfg.MediaBatchTwoPhase = true
		cfg.SecretPatterns = patterns
		cfg.RetrievalHierarchicalEnabled = true
		cfg.RetrievalHierarchicalLevels = []string{config.HierarchicalLevelDocument}
		cfg.RetrievalHierarchicalSourceReps = []string{"transcript", "transcript-en"}
		cfg.RetrievalHierarchicalMaxTokens = config.DefaultHierarchicalMaxTokens
		cfg.RetrievalHierarchicalPromptVersion = config.DefaultHierarchicalPromptVersion
		svc := mustNewIngestService(t, cfg, st)
		svc.SetTranscriber(&fakeTranscriber{text: "[00:00] the report covers the regional budget and the new bridge"})
		svc.SetSTTIdentity("whisper", "large-v3")
		svc.SetTranscriptLanguage("de")
		svc.SetTranslator(&codenameTranslator{}, "mistral", "model-a", []string{"en"})
		sum := &recordingSummarizer{}
		svc.SetSummarizer(sum, "mistral", "mistral-small-latest")
		if err := svc.Run(ctx); err != nil {
			t.Fatalf("Run: %v", err)
		}
		return sum
	}

	defaults := config.Default().SecretPatterns
	run(defaults)

	broader := append(append([]string(nil), defaults...), `BLUEHERON-[a-z]+-[0-9]{4}`)
	third := run(broader)
	third.mu.Lock()
	defer third.mu.Unlock()
	for _, p := range third.prompts {
		if strings.Contains(p, "BLUEHERON") {
			t.Fatalf("the summarizer received the unchecked translation after a broader policy change:\n%s", p)
		}
	}
	requireStatus(t, st, rel, model.DocumentStatusSecretExcluded)
}

// codenameSummarizer returns a summary that holds a codename.
type codenameSummarizer struct {
	recordingSummarizer
}

func (c *codenameSummarizer) Generate(ctx context.Context, prompt string) (string, error) {
	if _, err := c.recordingSummarizer.Generate(ctx, prompt); err != nil {
		return "", err
	}
	return "summary: project BLUEHERON-alpha-2026 covers the budget", nil
}

// TestSecretPolicy_SummaryMatchWithholdsTheDocument pins the settle path of
// the summary step (SPEC §7.2). A summary source or a summary output that
// matches the policy withholds the whole document: it is settled as
// secret_excluded with no live representation, its pending mark is not
// cleared by an "ok" finalize, and no summary is persisted.
func TestSecretPolicy_SummaryMatchWithholdsTheDocument(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	codename := `BLUEHERON-[a-z]+-[0-9]{4}`

	t.Run("summary output", func(t *testing.T) {
		root := testutil.TempDir(t)
		writeFile(t, filepath.Join(root, "notes.txt"), "the report covers the regional budget and the new bridge")
		st := newRealStore(t)
		cfg := hierarchicalConfig(testutil.TempDir(t))
		cfg.RootDir = root
		cfg.STTProvider = "off"
		cfg.SecretPatterns = append(append([]string(nil), config.Default().SecretPatterns...), codename)
		svc := mustNewIngestService(t, cfg, st)
		svc.SetSummarizer(&codenameSummarizer{}, "mistral", "mistral-small-latest")
		if err := svc.Run(ctx); err != nil {
			t.Fatalf("Run: %v", err)
		}
		requireWithheldAndRetired(t, st, "notes.txt")
		// The rejected summary is not in the summary cache.
		requireNoCachedText(t, filepath.Join(cfg.StateDir, "cache", "summary"), "BLUEHERON")
	})

	t.Run("summary output cached by an earlier run", func(t *testing.T) {
		root := testutil.TempDir(t)
		writeFile(t, filepath.Join(root, "notes.txt"), "the report covers the regional budget and the new bridge")
		st := newRealStore(t)
		cfg := hierarchicalConfig(testutil.TempDir(t))
		cfg.RootDir = root
		cfg.STTProvider = "off"
		// First run: the codename is not a secret yet, so the summary is cached.
		first := mustNewIngestService(t, cfg, st)
		first.SetSummarizer(&codenameSummarizer{}, "mistral", "mistral-small-latest")
		if err := first.Run(ctx); err != nil {
			t.Fatalf("Run: %v", err)
		}
		cacheDir := filepath.Join(cfg.StateDir, "cache", "summary")
		entries, err := os.ReadDir(cacheDir)
		if err != nil || len(entries) == 0 {
			t.Fatalf("the first run cached no summary (%v); the test proves nothing", err)
		}
		// The pattern arrives. The cached summary matches: the document is
		// withheld and the entry is removed.
		cfg.SecretPatterns = append(append([]string(nil), config.Default().SecretPatterns...), codename)
		second := mustNewIngestService(t, cfg, st)
		second.SetSummarizer(&codenameSummarizer{}, "mistral", "mistral-small-latest")
		if err := second.Run(ctx); err != nil {
			t.Fatalf("Run: %v", err)
		}
		requireWithheldAndRetired(t, st, "notes.txt")
		requireNoCachedText(t, cacheDir, "BLUEHERON")
	})

	t.Run("summary source", func(t *testing.T) {
		root := testutil.TempDir(t)
		mustWriteFile(t, filepath.Join(root, "media", "clip.mp3"), []byte("fake-audio"))
		stateDir := testutil.TempDir(t)
		st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
		if err := st.Init(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		run := func(patterns, targets []string, sum *recordingSummarizer) {
			t.Helper()
			cfg := config.Default()
			cfg.RootDir = root
			cfg.StateDir = stateDir
			cfg.STTProvider = "off"
			cfg.SecretPatterns = patterns
			cfg.RetrievalHierarchicalEnabled = true
			cfg.RetrievalHierarchicalLevels = []string{config.HierarchicalLevelDocument}
			cfg.RetrievalHierarchicalSourceReps = []string{"transcript-en"}
			cfg.RetrievalHierarchicalMaxTokens = config.DefaultHierarchicalMaxTokens
			cfg.RetrievalHierarchicalPromptVersion = config.DefaultHierarchicalPromptVersion
			svc := mustNewIngestService(t, cfg, st)
			svc.SetTranscriber(&fakeTranscriber{text: "[00:00] the report covers the regional budget and the new bridge"})
			svc.SetSTTIdentity("whisper", "large-v3")
			svc.SetTranscriptLanguage("de")
			if len(targets) > 0 {
				svc.SetTranslator(&codenameTranslator{}, "mistral", "model-a", targets)
			}
			svc.SetSummarizer(sum, "mistral", "mistral-small-latest")
			if err := svc.Run(ctx); err != nil {
				t.Fatalf("Run: %v", err)
			}
		}
		defaults := config.Default().SecretPatterns
		// Single pass: the translation holds the codename.
		run(defaults, []string{"en"}, &recordingSummarizer{})
		requireStatus(t, st, "media/clip.mp3", model.DocumentStatusOK)
		// A broader pattern, and translation switched off. The scan re-reads
		// the document; the stored translation is not made again, and the
		// summary step reads it before the output reconciliation retires it.
		broader := append(append([]string(nil), defaults...), codename)
		sum := &recordingSummarizer{}
		run(broader, nil, sum)
		sum.mu.Lock()
		for _, p := range sum.prompts {
			if strings.Contains(p, "BLUEHERON") {
				sum.mu.Unlock()
				t.Fatalf("the summarizer received a source that matches the policy:\n%s", p)
			}
		}
		sum.mu.Unlock()
		requireWithheldAndRetired(t, st, "media/clip.mp3")
	})
}

// requireNoCachedText fails when a file in dir holds text.
func requireNoCachedText(t *testing.T, dir, text string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if strings.Contains(string(raw), text) {
			t.Errorf("cache entry %s holds the rejected text", e.Name())
		}
	}
}
