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

func partialTranscriptCoverage(ctx context.Context, db *sql.DB) (model.TranscriptCoverageSummary, error) {
	// The `%coverage%` predicate is a cheap pre-filter, not the decision: it
	// keeps the scan off every single-request transcript in a large corpus, and
	// Complete below decides. A false positive costs one JSON decode.
	rows, err := db.QueryContext(ctx, `
		SELECT r.meta_json
		FROM representations r
		JOIN documents d ON d.doc_id = r.doc_id
		WHERE r.deleted = 0 AND d.deleted = 0
		  AND r.rep_type = 'transcript'
		  AND r.meta_json LIKE '%coverage%'`)
	if err != nil {
		return model.TranscriptCoverageSummary{}, err
	}
	defer func() { _ = rows.Close() }()

	var out model.TranscriptCoverageSummary
	seen := map[string]bool{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return model.TranscriptCoverageSummary{}, err
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
		out.Transcripts++
		if id := transcriptIdentity(meta); id != "" && !seen[id] {
			seen[id] = true
			out.Providers = append(out.Providers, id)
		}
		if cov.DurationMS <= 0 {
			// The duration probe failed (§8.6.13). Counted as a file, excluded
			// from the length, and reported: an unknown length summed as zero
			// would report a shortfall of nothing, which is the silence the
			// report exists to remove.
			out.UnknownDuration++
			continue
		}
		out.DecodedMS += int64(cov.DecodedMS)
		out.DurationMS += int64(cov.DurationMS)
	}
	if err := rows.Err(); err != nil {
		return model.TranscriptCoverageSummary{}, err
	}
	sort.Strings(out.Providers)
	return out, nil
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
