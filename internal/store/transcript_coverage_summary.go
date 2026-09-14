package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"

	"github.com/dirstral/dir2mcp/internal/model"
)

// transcriptCoverageMeta is the part of a transcript representation's meta_json
// this aggregate reads. Only `coverage` is decoded: a transcript meta_json also
// carries a TOP-LEVEL duration_ms, which is the media's length and not a decode
// result, and summing that one would report a shortfall against the wrong
// denominator (SPEC §7.7, dirstral-spec#107 review).
type transcriptCoverageMeta struct {
	Coverage *model.TranscriptCoverage `json:"coverage"`
	// The §8.6.7 derivation identity, which is what the §7.7 remediation is
	// allowed to name. The ENDPOINT is deliberately absent: one provider may be
	// routed to several, and which one served a given window is not recorded, so
	// a report that named an endpoint would be guessing (dirstral-spec#107).
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// PartialTranscriptCoverage aggregates the §8.6.13 coverage records that do NOT
// state completeness, for the §7.7 honest-coverage report (#972).
//
// Why this exists at all. #961 made a windowed decode record which part of a
// recording it actually produced text for, per representation. Nothing summed
// it. `media.stt.on_partial_transcript` defaults to `warn`, which PERSISTS the
// partial transcript and leaves its document `status=ok`, so the default path
// reaches no other report: `skip_reasons` sees only the `skip` path, and the
// extraction verdict is scoped to format classes. A corpus can therefore be
// 100% indexed, report no skips and no errors, and still be missing hours of
// speech.
//
// Live representations only. A transcript retired by a partial-transcript
// refusal (§8.6.13) is tombstoned with `deleted = 1` and its chunks are gone, so
// counting it would report a shortfall for audio that is no longer indexed at
// all; that case is already the `skip_reasons` aggregate's.
//
// The whole transcript rep_type FAMILY is walked, not the bare `transcript`.
// §8.6.12 gives an additional audio track its own `transcript@t<N>` rep_type
// (UNIQUE(doc_id, rep_type) makes that the only way to hold more than one), and
// each track is a separate decode with its own coverage. A predicate matching
// only `transcript` would report a multi-track recording as fully covered while
// every track past the first was missing most of its speech. Translations
// (`transcript-<lang>`) and sidecars are matched too and fall out on their own,
// because neither is a windowed decode and neither records a `coverage` object.
//
// Rows are decoded in Go rather than with json_extract so that completeness is
// decided by model.TranscriptCoverage.Complete, the one definition ingest writes
// with. A meta_json that does not parse, or that carries no `coverage`, is not
// counted: §5.2 absence is "no assertion", and a single-request decode records
// nothing precisely so that absence cannot be read as either complete or
// partial.
func (s *SQLiteStore) PartialTranscriptCoverage(ctx context.Context) (model.TranscriptCoverageSummary, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return model.TranscriptCoverageSummary{}, err
	}
	defer s.ReleaseDB()
	return partialTranscriptCoverage(ctx, db)
}

// PartialTranscriptPaths returns the rel_paths of the documents whose live
// transcript records incomplete coverage, sorted, for the re-decode action the
// §7.7 remediation names (#974).
//
// It walks the same rows as PartialTranscriptCoverage through the same
// predicate, so the corpus the report COUNTS and the corpus the action REPAIRS
// cannot drift apart. A report that names 12 transcripts and an action that
// repairs 9 of them would be a worse failure than the silence both exist to
// remove.
func (s *SQLiteStore) PartialTranscriptPaths(ctx context.Context) ([]string, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	// One document can hold SEVERAL partial transcripts: §8.6.12 gives every
	// additional audio track its own `transcript@t<N>`. The report counts those
	// separately, because each is its own decode with its own missing audio, but
	// the repair is per DOCUMENT, and a run that announced "re-decoding 4
	// recordings" for two files would misreport what it is about to do.
	var paths []string
	seen := map[string]bool{}
	if err := walkPartialTranscripts(ctx, db, func(relPath string, _ *model.TranscriptCoverage) {
		if seen[relPath] {
			return
		}
		seen[relPath] = true
		paths = append(paths, relPath)
	}); err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

func partialTranscriptCoverage(ctx context.Context, db *sql.DB) (model.TranscriptCoverageSummary, error) {
	var out model.TranscriptCoverageSummary
	seen := map[string]bool{}
	err := walkPartialTranscripts(ctx, db, func(_ string, cov *model.TranscriptCoverage) {
		out.Transcripts++
		if id := cov.Identity; id != "" && !seen[id] {
			seen[id] = true
			out.Providers = append(out.Providers, id)
		}
		if cov.DurationMS <= 0 {
			// The duration probe failed (§8.6.13). Counted as a file, excluded
			// from the length, and reported: an unknown length summed as zero
			// would report a shortfall of nothing, which is the silence the
			// report exists to remove.
			out.UnknownDuration++
			return
		}
		out.DecodedMS += int64(cov.DecodedMS)
		out.DurationMS += int64(cov.DurationMS)
	})
	if err != nil {
		return model.TranscriptCoverageSummary{}, err
	}
	noAssertion, err := countTranscriptsWithoutCoverage(ctx, db)
	if err != nil {
		return model.TranscriptCoverageSummary{}, err
	}
	out.NoAssertion = noAssertion
	sort.Strings(out.Providers)
	return out, nil
}

// countTranscriptsWithoutCoverage counts the live DECODED transcripts that carry
// no §8.6.13 coverage record at all (#977).
//
// §5.2 says an absent field is "no assertion", and the report has to say so
// rather than fold it into the clean count. A corpus indexed before the record
// existed has zero coverage objects, so a check that only counts INCOMPLETE
// coverage reports "no transcript records an incomplete decode" — the same
// sentence a genuinely clean corpus gets. Measured on the RFE validation
// corpus: 34 transcripts, 0 coverage records, and 19 of the 50 recordings large
// enough that a decode today would be windowed.
//
// Counted in SQL rather than by decoding every transcript's meta_json, because
// this runs on the `up` banner and an archive holds a transcript per recording.
//
// The membership tests are SQLite JSON1 functions, not LIKE patterns. A LIKE has
// to encode an assumption about how the writer formatted the document, and both
// spellings of that assumption are wrong in a way that UNDER-reports:
// `%"coverage"%` also matches the word as a VALUE (`track_label` carries the
// container's track title per §8.6.12, and "coverage" is an ordinary broadcast
// word), while `%"coverage":%` misses a document that puts a space before the
// colon. json_extract asks the question that is actually meant.
//
// Each test is guarded by json_valid, because json_extract raises on a document
// it cannot parse and one unreadable row must not fail a startup banner. A row
// whose meta cannot be read is COUNTED here: it certainly carries no coverage
// record, and it cannot be shown to be a sidecar or a translation either, so
// excluding it would be the same silence this count exists to remove.
//
// Two populations are excluded because neither is a windowed decode and neither
// could ever carry coverage: a SIDECAR transcript is authored rather than
// decoded, and a TRANSLATION derives from another transcript's text. Counting
// them would inflate the number with rows whose silence means nothing.
func countTranscriptsWithoutCoverage(ctx context.Context, db *sql.DB) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM representations r
		JOIN documents d ON d.doc_id = r.doc_id
		WHERE r.deleted = 0 AND d.deleted = 0
		  AND (r.rep_type = 'transcript'
		       OR r.rep_type LIKE 'transcript@%'
		       OR r.rep_type LIKE 'transcript-%')
		  AND NOT (json_valid(r.meta_json) AND json_extract(r.meta_json, '$.coverage') IS NOT NULL)
		  AND NOT (json_valid(r.meta_json) AND json_extract(r.meta_json, '$.source') = 'sidecar')
		  AND NOT (json_valid(r.meta_json) AND json_extract(r.meta_json, '$.translate_provider') IS NOT NULL)`).Scan(&n)
	return n, err
}

// walkPartialTranscripts calls visit once per LIVE transcript representation
// whose §8.6.13 coverage record does not state completeness. It is the single
// definition of "partial" that both the report and the re-decode action read.
//
// A transcript retired by a partial-transcript refusal is tombstoned with
// `deleted = 1` and its chunks are gone, so it is not walked: reporting a
// shortfall for audio that is no longer indexed at all is the `skip_reasons`
// aggregate's job, not this one.
//
// Rows are decoded in Go rather than with json_extract so completeness is
// decided by model.TranscriptCoverage.Complete, the one definition ingest writes
// with. A meta_json that does not parse, or that carries no `coverage`, is
// skipped: §5.2 absence is "no assertion", and a single-request decode records
// nothing precisely so that absence cannot be read as either complete or partial.
func walkPartialTranscripts(ctx context.Context, db *sql.DB, visit func(relPath string, cov *model.TranscriptCoverage)) error {
	// The `%coverage%` predicate is a cheap pre-filter, not the decision: it
	// keeps the scan off every single-request transcript in a large corpus, and
	// Complete below decides. A false positive costs one JSON decode.
	rows, err := db.QueryContext(ctx, `
		SELECT d.rel_path, r.meta_json
		FROM representations r
		JOIN documents d ON d.doc_id = r.doc_id
		WHERE r.deleted = 0 AND d.deleted = 0
		  AND (r.rep_type = 'transcript'
		       OR r.rep_type LIKE 'transcript@%'
		       OR r.rep_type LIKE 'transcript-%')
		  AND r.meta_json LIKE '%coverage%'`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var relPath, raw string
		if err := rows.Scan(&relPath, &raw); err != nil {
			return err
		}
		var meta transcriptCoverageMeta
		if err := json.Unmarshal([]byte(raw), &meta); err != nil {
			// Not this aggregate's business to fail a startup banner over one
			// unreadable row, and a row it cannot read asserts nothing.
			continue
		}
		cov := meta.Coverage
		if cov == nil || cov.WindowsAttempted <= 0 || cov.Complete() {
			continue
		}
		cov.Identity = transcriptIdentity(meta)
		visit(relPath, cov)
	}
	return rows.Err()
}

// transcriptIdentity renders the recorded derivation identity for the report,
// as "provider/model", "provider" or "" when neither is recorded. A transcript
// from before the identity was recorded contributes nothing rather than an
// invented name.
func transcriptIdentity(meta transcriptCoverageMeta) string {
	provider := strings.TrimSpace(meta.Provider)
	modelName := strings.TrimSpace(meta.Model)
	switch {
	case provider != "" && modelName != "":
		return provider + "/" + modelName
	case provider != "":
		return provider
	default:
		return modelName
	}
}
