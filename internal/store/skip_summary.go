package store

import (
	"context"
	"database/sql"

	"github.com/dirstral/dir2mcp/internal/model"
)

// skipSummaryMaxSamples bounds how many representative skipped documents we
// surface to status / reindex / support-bundle consumers. Small on purpose:
// the diagnostic value comes from spotting *which* reason dominates coverage
// gaps, not from dumping every skipped rel_path.
const skipSummaryMaxSamples = 10

// loadSkipSummary aggregates never-indexed (skipped) documents by skip_reason
// and collects up to maxSamples representative {rel_path, reason} rows. It is
// the durable half of the honest-coverage surface (#414/#395): only documents
// persisted with status IN ('skipped','secret_excluded') contribute — the
// same lifecycle set CorpusStats counts as Skipped. Returns nil when nothing
// was skipped so CorpusStats.SkipSummary stays omitted from the JSON.
func loadSkipSummary(ctx context.Context, db *sql.DB, maxSamples int) (*model.SkipSummary, error) {
	categories, err := loadSkipCategories(ctx, db)
	if err != nil {
		return nil, err
	}
	if len(categories) == 0 {
		return nil, nil
	}
	samples, err := loadSkipSamples(ctx, db, maxSamples)
	if err != nil {
		return nil, err
	}
	return &model.SkipSummary{Categories: categories, Samples: samples}, nil
}

// loadSkipCategories returns a count of skipped documents per skip_reason. An
// empty reason (rows persisted before the column existed, or a skip site that
// left it unset) is normalized to "unknown" so consumers don't have to.
func loadSkipCategories(ctx context.Context, db *sql.DB) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(NULLIF(skip_reason, ''), 'unknown') AS reason, COUNT(*) AS n
		FROM documents
		WHERE deleted = 0 AND status IN ('skipped', 'secret_excluded')
		GROUP BY reason
		ORDER BY n DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]int64{}
	for rows.Next() {
		var reason string
		var count int64
		if err := rows.Scan(&reason, &count); err != nil {
			return nil, err
		}
		out[reason] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// loadSkipSamples returns up to maxSamples skipped documents with rel_path /
// reason. Ordered by reason then rel_path so the sample distribution favours
// coverage across reasons rather than dumping the first N of one type.
func loadSkipSamples(ctx context.Context, db *sql.DB, maxSamples int) ([]model.SkipSample, error) {
	if maxSamples <= 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT rel_path, COALESCE(NULLIF(skip_reason, ''), 'unknown') AS reason
		FROM documents
		WHERE deleted = 0 AND status IN ('skipped', 'secret_excluded')
		ORDER BY reason, rel_path
		LIMIT ?`, maxSamples)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]model.SkipSample, 0, maxSamples)
	for rows.Next() {
		var sample model.SkipSample
		if err := rows.Scan(&sample.RelPath, &sample.Reason); err != nil {
			return nil, err
		}
		out = append(out, sample)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
