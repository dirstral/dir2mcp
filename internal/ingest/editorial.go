package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/avutil"
	"github.com/dirstral/dir2mcp/internal/editorial"
	"github.com/dirstral/dir2mcp/internal/langdetect"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/secretmatch"
	"github.com/dirstral/dir2mcp/internal/store"
)

// External editorial annotation import, apply side (SPEC §7.11).
//
// `dir2mcp annotate import` only validates a file and stages it under
// imports/. The process that writes the index applies it: the scan's
// finishScan step and, in a running daemon, the staged-import poller. Both go
// through runEditorialPass, which takes one lock, so imports are applied one at
// a time, in staging order.
//
// A pass has two halves:
//
//  1. Apply each staged import: resolve every record's key against the
//     eligible documents, keep the applied records under imports/records,
//     replace that source's editorial representation on each document, write
//     the report and emit it, then remove the staged file.
//  2. Rebuild from the retained records: a record whose document was
//     tombstoned is removed; a representation whose inputs changed (the media
//     duration, the name folds) or that a reindex must embed again is built
//     again from the retained record, with no new import.
//
// The representation is a deterministic function of the source name, the
// record, the media duration and the name folds, and its rep_hash covers
// exactly those, so an unchanged input is never written or embedded again.

// editorialStore is the store capability the apply step needs. The SQLite store
// implements it; a store without it disables the import (logged once per pass).
type editorialStore interface {
	ListEditorialDocuments(ctx context.Context) ([]store.EditorialDocument, error)
	ListEditorialRepresentations(ctx context.Context) (map[int64]map[string]store.EditorialRepresentation, error)
}

// editorialState is the Service's editorial import state.
type editorialState struct {
	mu        sync.Mutex
	memoMu    sync.Mutex
	durations map[string]editorialDurationMemo
	onReport  func(level string, report editorial.Report)
	reportMu  sync.RWMutex
	// reindexing is set while Reindex runs.
	reindexing atomic.Bool
}

// editorialDurationMemo records a probed media duration for the content it was
// probed on, so a pass probes each document at most once per content.
type editorialDurationMemo struct {
	contentHash string
	duration    editorial.Duration
	probedAt    time.Time
}

// editorialUnknownDurationRetry is how long an unknown duration is reused
// before the media is probed again. A known duration is reused for as long as
// the content is unchanged.
const editorialUnknownDurationRetry = 10 * time.Minute

// editorialMeta is the meta_json of an editorial representation (SPEC §7.11.4).
// It records no provider or model: the representation is authored data.
type editorialMeta struct {
	Source             string          `json:"source"`
	KeyKind            string          `json:"key_kind"`
	Key                string          `json:"key"`
	Match              json.RawMessage `json:"match,omitempty"`
	RecordHash         string          `json:"record_hash"`
	ImportedUnix       int64           `json:"imported_unix"`
	NameFolds          []string        `json:"name_folds"`
	Language           string          `json:"language,omitempty"`
	LanguageSource     string          `json:"language_source,omitempty"`
	LanguageConfidence float64         `json:"language_confidence,omitempty"`
}

// SetOnEditorialImport registers the callback that receives each import report
// when the import is applied. The CLI emits it as the `editorial_import_applied`
// event (SPEC §7.11.5).
func (s *Service) SetOnEditorialImport(fn func(level string, report editorial.Report)) {
	s.editorial.reportMu.Lock()
	defer s.editorial.reportMu.Unlock()
	s.editorial.onReport = fn
}

// HasStagedEditorialImports reports whether a staged import waits to be
// applied. A read error reports false; the next scan pass tries again.
func (s *Service) HasStagedEditorialImports() bool {
	if strings.TrimSpace(s.cfg.StateDir) == "" {
		return false
	}
	paths, err := editorial.ListStaged(s.cfg.StateDir)
	return err == nil && len(paths) > 0
}

// ApplyEditorialImports applies every staged import and rebuilds the retained
// records whose inputs changed. A running daemon calls it when an import is
// staged between scans; every scan calls it at its end.
func (s *Service) ApplyEditorialImports(ctx context.Context) error {
	return s.runEditorialPass(ctx, false)
}

// isFullReindex reports whether this scan is a full reindex. A reindex moves
// the vector index aside, so the editorial chunks must be written and embedded
// again even when their inputs did not change.
func (s *Service) isFullReindex() bool {
	if s.editorial.reindexing.Load() {
		return true
	}
	return s.indexingState != nil && s.indexingState.Snapshot().Mode == appstate.ModeFull
}

// runEditorialPassLogged is runEditorialPass for the scan: an error is logged,
// never returned, because the scan itself succeeded.
func (s *Service) runEditorialPassLogged(ctx context.Context, force bool) {
	if err := s.runEditorialPass(ctx, force); err != nil && ctx.Err() == nil {
		s.getLogger().Printf("editorial import: %v", err)
	}
}

// runEditorialPass is one apply pass: staged imports first, then the rebuild
// of the retained records. force rewrites every representation.
func (s *Service) runEditorialPass(ctx context.Context, force bool) error {
	es, ok := s.store.(editorialStore)
	if !ok || s.repGen == nil || strings.TrimSpace(s.cfg.StateDir) == "" {
		return nil
	}
	s.editorial.mu.Lock()
	defer s.editorial.mu.Unlock()
	folds, err := editorial.ParseFolds(s.cfg.AnnotationsNameFolds)
	if err != nil {
		return fmt.Errorf("annotations.name_folds: %w", err)
	}
	matcher, err := compileSecretMatcher(s.cfg)
	if err != nil {
		return err
	}
	paths, err := editorial.ListStaged(s.cfg.StateDir)
	if err != nil {
		return fmt.Errorf("list staged imports: %w", err)
	}
	for _, path := range paths {
		if err := s.applyStagedImport(ctx, es, path, folds, matcher); err != nil {
			return err
		}
	}
	// A reindex leaves a marker until a forced rebuild succeeds, so a failed
	// forced pass is retried by the next pass, also after a restart.
	force = force || editorial.RebuildPending(s.cfg.StateDir)
	if err := s.rebuildRetainedEditorial(ctx, es, folds, matcher, force); err != nil {
		return err
	}
	if force {
		return editorial.ClearRebuildPending(s.cfg.StateDir)
	}
	return nil
}

// markEditorialRebuildPending records that every editorial representation
// must be written again, because a reindex moved the vector index aside.
func (s *Service) markEditorialRebuildPending() {
	if strings.TrimSpace(s.cfg.StateDir) == "" {
		return
	}
	if err := editorial.MarkRebuildPending(s.cfg.StateDir); err != nil {
		s.getLogger().Printf("editorial import: mark rebuild pending: %v", err)
	}
}

// editorialRun carries what one apply needs about the import and the corpus.
type editorialRun struct {
	source  string
	keyKind string
	folds   editorial.Folds
	reps    map[int64]map[string]store.EditorialRepresentation
}

// applyStagedImport applies one staged import, records and emits its report,
// and removes the staged file. A staged file that cannot be read is renamed
// aside so it cannot block every later import.
func (s *Service) applyStagedImport(ctx context.Context, es editorialStore, path string, folds editorial.Folds, matcher *secretmatch.Matcher) error {
	staged, records, err := readStagedRecords(path)
	if err != nil {
		s.getLogger().Printf("editorial import: %v; the file is set aside as %s.invalid", err, filepath.Base(path))
		_ = os.Rename(path, path+".invalid")
		return nil
	}
	docs, err := es.ListEditorialDocuments(ctx)
	if err != nil {
		return fmt.Errorf("list documents: %w", err)
	}
	reps, err := es.ListEditorialRepresentations(ctx)
	if err != nil {
		return fmt.Errorf("list editorial representations: %w", err)
	}
	run := editorialRun{source: staged.Source, keyKind: staged.Key, folds: folds, reps: reps}
	report := editorial.NewReport(staged.Source, staged.Key, len(records), staged.IgnoredFields)
	plan := planEditorialImport(records, newEditorialResolver(docs, staged.Key), &report)
	now := time.Now()
	for i, rec := range records {
		if err := s.applyPlannedRecord(ctx, run, rec, plan.targets[i], plan.conflicted, matcher, &report, now); err != nil {
			return err
		}
	}
	report.Finish(now)
	if _, err := editorial.SaveReport(s.cfg.StateDir, path, report); err != nil {
		return fmt.Errorf("record import report: %w", err)
	}
	s.emitEditorialReport(report)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove applied staged import: %w", err)
	}
	return nil
}

// readStagedRecords reads a staged import and re-validates its records.
func readStagedRecords(path string) (editorial.Staged, []editorial.Record, error) {
	staged, err := editorial.ReadStaged(path)
	if err != nil {
		return editorial.Staged{}, nil, err
	}
	records := make([]editorial.Record, 0, len(staged.Records))
	for i, raw := range staged.Records {
		rec, perr := editorial.ParseRecord(raw, staged.Key, staged.Source)
		if perr != nil {
			return editorial.Staged{}, nil, fmt.Errorf("staged import %s: record %d: %w", filepath.Base(path), i+1, perr)
		}
		records = append(records, rec)
	}
	return staged, records, nil
}

// applyPlannedRecord applies one record to each document it resolved to,
// except a document that another record of the import also resolved to.
func (s *Service) applyPlannedRecord(ctx context.Context, run editorialRun, rec editorial.Record, targets []store.EditorialDocument,
	conflicted map[string]struct{}, matcher *secretmatch.Matcher, report *editorial.Report, now time.Time) error {
	if len(targets) == 0 {
		return nil
	}
	if editorialRecordMatchesSecret(rec, matcher) {
		report.WithheldKeys = append(report.WithheldKeys, rec.Key)
		return nil
	}
	for _, doc := range targets {
		if _, skip := conflicted[doc.RelPath]; skip {
			continue
		}
		annotations, dropped, err := s.applyEditorialRecord(ctx, run, doc, rec, now)
		if err != nil {
			return fmt.Errorf("apply record %q to %s: %w", rec.Key, doc.RelPath, err)
		}
		report.Applied++
		report.Annotations += annotations
		report.SegmentsDropped += dropped
	}
	return nil
}

// editorialPlan is the resolution of one import: the eligible documents of
// each record, and the documents that two or more records resolved to.
type editorialPlan struct {
	targets    [][]store.EditorialDocument
	conflicted map[string]struct{}
}

// planEditorialImport resolves every key (SPEC §7.11.3) and counts the
// unmatched, non-media-only, multi-matched and conflicting keys.
func planEditorialImport(records []editorial.Record, resolver editorialResolver, report *editorial.Report) editorialPlan {
	plan := editorialPlan{targets: make([][]store.EditorialDocument, len(records)), conflicted: map[string]struct{}{}}
	claims := map[string][]int{}
	for i, rec := range records {
		matched, eligible := resolver.resolve(rec.Key)
		switch {
		case matched == 0:
			report.UnmatchedKeys = append(report.UnmatchedKeys, rec.Key)
		case len(eligible) == 0:
			report.NonMediaOnlyKeys = append(report.NonMediaOnlyKeys, rec.Key)
		default:
			plan.targets[i] = eligible
			if len(eligible) > 1 {
				report.MultiMatched++
			}
			for _, d := range eligible {
				claims[d.RelPath] = append(claims[d.RelPath], i)
			}
		}
	}
	conflictingKeys := map[string]struct{}{}
	for relPath, idx := range claims {
		if len(idx) < 2 {
			continue
		}
		plan.conflicted[relPath] = struct{}{}
		for _, i := range idx {
			conflictingKeys[records[i].Key] = struct{}{}
		}
	}
	for key := range conflictingKeys {
		report.ConflictingKeys = append(report.ConflictingKeys, key)
	}
	return plan
}

// editorialRecordMatchesSecret is the §7.2 secret check over the record's text.
func editorialRecordMatchesSecret(rec editorial.Record, matcher *secretmatch.Matcher) bool {
	if matcher == nil || !matcher.Enabled() {
		return false
	}
	for _, text := range rec.Texts() {
		if strings.TrimSpace(text) != "" && matcher.MatchString(text) {
			return true
		}
	}
	return false
}

// applyEditorialRecord keeps the record for rebuilds and replaces the source's
// representation on the document. A record with no content removes it.
func (s *Service) applyEditorialRecord(ctx context.Context, run editorialRun, doc store.EditorialDocument, rec editorial.Record, now time.Time) (int, int, error) {
	if !rec.HasContent() {
		if err := editorial.RemoveRetained(s.cfg.StateDir, doc.RelPath, run.source); err != nil {
			return 0, 0, err
		}
		return 0, 0, s.retireEditorial(ctx, doc, run.source, run.reps)
	}
	retained := editorial.Retained{
		Source: run.source, KeyKind: run.keyKind, RelPath: doc.RelPath,
		Record: rec.Canonical, RecordHash: rec.Hash, ImportedUnix: now.Unix(),
	}
	// The same record imported again keeps its first import time, so the
	// representation stays byte-identical and is not written again.
	prev, found, err := editorial.LoadRetained(s.cfg.StateDir, doc.RelPath, run.source)
	if err != nil {
		return 0, 0, err
	}
	if found && prev.RecordHash == rec.Hash && prev.KeyKind == run.keyKind {
		retained.ImportedUnix = prev.ImportedUnix
	} else if err := editorial.SaveRetained(s.cfg.StateDir, retained); err != nil {
		return 0, 0, fmt.Errorf("retain record: %w", err)
	}
	return s.buildEditorial(ctx, doc, retained, rec, run.folds, run.reps, false)
}

// rebuildRetainedEditorial is the second half of a pass: it drops the records
// of tombstoned documents and builds again each representation whose inputs
// changed (SPEC §7.11.3).
func (s *Service) rebuildRetainedEditorial(ctx context.Context, es editorialStore, folds editorial.Folds, matcher *secretmatch.Matcher, force bool) error {
	retained, bad, err := editorial.LoadAllRetained(s.cfg.StateDir)
	if err != nil {
		return fmt.Errorf("load retained records: %w", err)
	}
	for _, name := range bad {
		s.getLogger().Printf("editorial import: retained record %s does not decode; it is skipped", name)
	}
	if len(retained) == 0 {
		return nil
	}
	docs, err := es.ListEditorialDocuments(ctx)
	if err != nil {
		return fmt.Errorf("list documents: %w", err)
	}
	byPath := make(map[string]store.EditorialDocument, len(docs))
	for _, d := range docs {
		byPath[d.RelPath] = d
	}
	reps, err := es.ListEditorialRepresentations(ctx)
	if err != nil {
		return fmt.Errorf("list editorial representations: %w", err)
	}
	for _, r := range retained {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.rebuildOneRetained(ctx, r, byPath, reps, folds, matcher, force); err != nil {
			return err
		}
	}
	return nil
}

// rebuildOneRetained handles one retained record during the rebuild half.
func (s *Service) rebuildOneRetained(ctx context.Context, r editorial.Retained, byPath map[string]store.EditorialDocument,
	reps map[int64]map[string]store.EditorialRepresentation, folds editorial.Folds, matcher *secretmatch.Matcher, force bool) error {
	doc, ok := byPath[r.RelPath]
	if !ok {
		// The document's tombstone removes the record (SPEC §7.11.3); the
		// tombstone already retired its representations.
		return editorial.RemoveRetained(s.cfg.StateDir, r.RelPath, r.Source)
	}
	if !editorialEligible(doc) {
		// Not indexed now (an error, a skip): leave the record and the
		// representation alone until the document is indexed again.
		return nil
	}
	rec, err := r.Parse()
	if err != nil {
		s.getLogger().Printf("editorial import: retained record for %s does not validate (%v); it is skipped", r.RelPath, err)
		return nil
	}
	if editorialRecordMatchesSecret(rec, matcher) {
		return s.retireEditorial(ctx, doc, r.Source, reps)
	}
	if _, _, err := s.buildEditorial(ctx, doc, r, rec, folds, reps, force); err != nil {
		return fmt.Errorf("rebuild editorial annotations of %s: %w", r.RelPath, err)
	}
	return nil
}

// buildEditorial writes the editorial representation of one (source,
// document) from its record, unless the stored one already has the same
// rep_hash and force is false. It returns the annotation count and the number
// of dropped segments.
func (s *Service) buildEditorial(ctx context.Context, doc store.EditorialDocument, r editorial.Retained, rec editorial.Record,
	folds editorial.Folds, reps map[int64]map[string]store.EditorialRepresentation, force bool) (int, int, error) {
	duration := s.editorialDuration(ctx, doc)
	built := editorial.Build(rec, duration, folds)
	segments := editorialChunkSegments(built, r.Source)
	if len(segments) == 0 {
		return 0, built.SegmentsDropped, s.retireEditorial(ctx, doc, r.Source, reps)
	}
	repType := editorial.RepTypeForSource(r.Source)
	repHash := editorial.RepHash(r.Source, rec.Hash, duration, folds, editorialChunkBudget())
	if stored, ok := reps[doc.DocID][repType]; ok && !force && !stored.Deleted && stored.RepHash == repHash {
		return len(segments), built.SegmentsDropped, nil
	}
	meta, err := s.editorialMetaJSON(r, rec, folds, segments)
	if err != nil {
		return 0, 0, err
	}
	rep := model.Representation{
		DocID: doc.DocID, RepType: repType, RepHash: repHash, MetaJSON: meta, CreatedUnix: time.Now().Unix(),
	}
	// One transaction per document: a reader sees the old set or the new set,
	// never a mix (SPEC §7.11.3).
	err = s.repGen.store.WithTx(ctx, func(tx model.RepresentationStore) error {
		repID, upsertErr := tx.UpsertRepresentation(ctx, rep)
		if upsertErr != nil {
			return fmt.Errorf("upsert editorial representation: %w", upsertErr)
		}
		return s.repGen.upsertChunksForRepresentationWithStore(ctx, tx, repID, "text", segments, quarantineDecision{})
	})
	if err != nil {
		return 0, 0, err
	}
	if reps[doc.DocID] == nil {
		reps[doc.DocID] = map[string]store.EditorialRepresentation{}
	}
	reps[doc.DocID][repType] = store.EditorialRepresentation{DocID: doc.DocID, RepType: repType, RepHash: repHash, MetaJSON: meta}
	return len(segments), built.SegmentsDropped, nil
}

// retireEditorial removes the source's editorial annotations from the
// document: the representation and its chunks are tombstoned in one
// transaction. A source with no stored representation is a no-op.
func (s *Service) retireEditorial(ctx context.Context, doc store.EditorialDocument, source string, reps map[int64]map[string]store.EditorialRepresentation) error {
	repType := editorial.RepTypeForSource(source)
	stored, ok := reps[doc.DocID][repType]
	if !ok || stored.Deleted {
		return nil
	}
	err := s.repGen.store.WithTx(ctx, func(tx model.RepresentationStore) error {
		repID, err := tx.UpsertRepresentation(ctx, model.Representation{
			DocID: doc.DocID, RepType: repType, RepHash: stored.RepHash, MetaJSON: stored.MetaJSON,
			CreatedUnix: time.Now().Unix(), Deleted: true,
		})
		if err != nil {
			return err
		}
		if err := tx.SoftDeleteChunksFromOrdinal(ctx, repID, 0); err != nil {
			return err
		}
		return s.repGen.persistRepresentationText(ctx, tx, repID, "", 0)
	})
	if err != nil {
		return fmt.Errorf("remove editorial annotations of %s: %w", doc.RelPath, err)
	}
	stored.Deleted = true
	reps[doc.DocID][repType] = stored
	return nil
}

// editorialChunkSegments turns the built annotations into chunk segments: one
// chunk per person or segment annotation, and the description chunked by the
// §7.5 text rules, each chunk with the document span. Every span carries the
// source name.
func editorialChunkSegments(built editorial.Built, source string) []chunkSegment {
	sources := []string{source}
	out := make([]chunkSegment, 0, len(built.Annotations)+1)
	for _, a := range built.Annotations {
		if strings.TrimSpace(a.Text) == "" {
			continue
		}
		out = append(out, chunkSegment{
			Text: a.Text,
			Span: model.Span{
				Kind: a.SpanKind, StartMS: int(a.StartMS), EndMS: int(a.EndMS),
				Entities: a.Entities, Event: a.Event, Attributes: a.Attributes, Sources: sources,
			},
		})
	}
	if built.Description == "" {
		return out
	}
	for _, c := range chunkTextByChars(built.Description, effectiveTextChunkMaxChars, effectiveTextChunkOverlapChars, effectiveTextChunkMinChars) {
		if strings.TrimSpace(c.Text) == "" {
			continue
		}
		out = append(out, chunkSegment{
			Text: c.Text,
			Span: model.Span{
				Kind: editorial.SpanDocument, Event: editorial.EventDescription,
				Attributes: copyStringMap(built.DescriptionAttributes), Sources: sources,
			},
		})
	}
	return out
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// editorialChunkBudget names the text chunk budget the description is cut
// with, so a chunking change builds the representation again.
func editorialChunkBudget() string {
	return fmt.Sprintf("%d/%d/%d", effectiveTextChunkMaxChars, effectiveTextChunkOverlapChars, effectiveTextChunkMinChars)
}

// editorialMetaJSON renders the representation's meta_json (SPEC §7.11.4).
func (s *Service) editorialMetaJSON(r editorial.Retained, rec editorial.Record, folds editorial.Folds, segments []chunkSegment) (string, error) {
	meta := editorialMeta{
		Source: r.Source, KeyKind: r.KeyKind, Key: rec.Key, RecordHash: rec.Hash,
		ImportedUnix: r.ImportedUnix, NameFolds: folds.Entries(),
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rec.Canonical, &fields); err == nil {
		if m, ok := fields["match"]; ok && string(m) != "null" {
			meta.Match = m
		}
	}
	if lang := strings.TrimSpace(rec.Language); lang != "" {
		meta.Language, meta.LanguageSource = lang, langSourceDeclared
	} else if s.repGen.langDetectEnabled {
		if tag, confidence, ok := langdetect.Detect(joinSegmentTexts(segments), langdetect.DefaultMinConfidence); ok {
			meta.Language, meta.LanguageSource, meta.LanguageConfidence = tag, langSourceDetected, confidence
		}
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return "", fmt.Errorf("marshal editorial meta: %w", err)
	}
	return string(encoded), nil
}

// editorialDuration returns the document's media duration (SPEC §7.11.4): a
// probe of the media, memoized per content so a pass probes each document once.
// A failed probe is an unknown duration, never an error.
func (s *Service) editorialDuration(ctx context.Context, doc store.EditorialDocument) editorial.Duration {
	s.editorial.memoMu.Lock()
	if memo, ok := s.editorial.durations[doc.RelPath]; ok && memo.contentHash == doc.ContentHash &&
		(memo.duration.Known || time.Since(memo.probedAt) < editorialUnknownDurationRetry) {
		s.editorial.memoMu.Unlock()
		return memo.duration
	}
	s.editorial.memoMu.Unlock()

	duration := s.probeEditorialDuration(ctx, doc.RelPath)
	s.editorial.memoMu.Lock()
	if s.editorial.durations == nil {
		s.editorial.durations = map[string]editorialDurationMemo{}
	}
	s.editorial.durations[doc.RelPath] = editorialDurationMemo{contentHash: doc.ContentHash, duration: duration, probedAt: time.Now()}
	s.editorial.memoMu.Unlock()
	return duration
}

// probeEditorialDuration probes one media file through the corpus filesystem.
func (s *Service) probeEditorialDuration(ctx context.Context, relPath string) editorial.Duration {
	localPath, cleanup, err := s.corpusFS().Localize(ctx, relPath)
	if err != nil {
		return editorial.Duration{}
	}
	defer cleanup()
	absPath, err := filepath.Abs(localPath)
	if err != nil {
		return editorial.Duration{}
	}
	probe := s.ProbeDurationFunc
	if probe == nil {
		probe = avutil.Duration
	}
	probeCtx, cancel := context.WithTimeout(ctx, recognizeProbeDurationTimeout)
	defer cancel()
	d, err := probe(probeCtx, absPath)
	if err != nil || d <= 0 {
		return editorial.Duration{}
	}
	return editorial.Duration{MS: d.Milliseconds(), Known: true}
}

// emitEditorialReport passes a report to the registered callback and logs it.
func (s *Service) emitEditorialReport(report editorial.Report) {
	s.getLogger().Printf("editorial import applied: source=%q records=%d applied=%d annotations=%d unmatched=%d non_media_only=%d conflicting=%d withheld=%d multi_matched=%d",
		report.Source, report.Records, report.Applied, report.Annotations, report.Unmatched, report.NonMediaOnly,
		report.Conflicting, report.Withheld, report.MultiMatched)
	s.editorial.reportMu.RLock()
	fn := s.editorial.onReport
	s.editorial.reportMu.RUnlock()
	if fn != nil {
		fn(report.Level(), report)
	}
}

// editorialEligible reports whether an editorial key may resolve to the
// document (SPEC §7.11.1): audio or video, indexed (status ok). Deleted
// documents never reach here.
func editorialEligible(doc store.EditorialDocument) bool {
	if doc.DocType != "audio" && doc.DocType != "video" {
		return false
	}
	return doc.Status == string(model.DocumentStatusOK)
}

// editorialResolver resolves a record key to documents (SPEC §7.11.1).
type editorialResolver struct {
	keyKind string
	byKey   map[string][]store.EditorialDocument
}

// newEditorialResolver indexes the documents by every value the key kind can
// match. For stem, a file name is indexed under itself and under each prefix
// that ends before a ".", "_" or "-", so a lookup is one map read.
func newEditorialResolver(docs []store.EditorialDocument, keyKind string) editorialResolver {
	r := editorialResolver{keyKind: keyKind, byKey: map[string][]store.EditorialDocument{}}
	for _, d := range docs {
		for _, k := range editorialKeysFor(d, keyKind) {
			r.byKey[k] = append(r.byKey[k], d)
		}
	}
	return r
}

// editorialKeysFor lists the key values that match the document.
func editorialKeysFor(d store.EditorialDocument, keyKind string) []string {
	switch keyKind {
	case editorial.KeyRelPath:
		return []string{d.RelPath}
	case editorial.KeyContentHash:
		if d.ContentHash == "" {
			return nil
		}
		return []string{d.ContentHash}
	}
	name := d.RelPath
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	keys := []string{name}
	seen := map[string]struct{}{name: {}}
	for i, r := range name {
		if r != '.' && r != '_' && r != '-' || i == 0 {
			continue
		}
		prefix := name[:i]
		if _, dup := seen[prefix]; !dup {
			seen[prefix] = struct{}{}
			keys = append(keys, prefix)
		}
	}
	return keys
}

// resolve returns how many documents the key matches at all, and the eligible
// ones among them.
func (r editorialResolver) resolve(key string) (int, []store.EditorialDocument) {
	matched := r.byKey[key]
	var eligible []store.EditorialDocument
	for _, d := range matched {
		if editorialEligible(d) {
			eligible = append(eligible, d)
		}
	}
	return len(matched), eligible
}
