package store

import (
	"context"
	"database/sql"

	"github.com/dirstral/dir2mcp/internal/model"
)

// failureSummaryMaxSamples bounds how many representative failing
// chunks we surface to status / doctor / support-bundle consumers.
// Small on purpose: the diagnostic value comes from spotting *which*
// kind of failure dominates, not from dumping every error string.
const failureSummaryMaxSamples = 10

// loadFailureSummary aggregates chunk-level embedding failures by
// error_category and collects up to maxSamples representative
// {rel_path, category, message} rows. Returns nil when there are no
// failures so CorpusStats.FailureSummary stays omitted in the JSON.
func loadFailureSummary(ctx context.Context, db *sql.DB, maxSamples int) (*model.FailureSummary, error) {
	categories, err := loadFailureCategories(ctx, db)
	if err != nil {
		return nil, err
	}
	if len(categories) == 0 {
		return nil, nil
	}
	samples, err := loadFailureSamples(ctx, db, maxSamples)
	if err != nil {
		return nil, err
	}
	return &model.FailureSummary{Categories: categories, Samples: samples}, nil
}

// loadFailureCategories returns a count of failed chunks per
// error_category. An empty category string (legacy failures recorded
// before the column existed, or via the unclassified MarkFailed entry
// point) is normalized to "unknown" so consumers don't have to.
func loadFailureCategories(ctx context.Context, db *sql.DB) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(NULLIF(error_category, ''), 'unknown') AS category, COUNT(*) AS n
		FROM chunks
		WHERE deleted = 0 AND embedding_status = 'error'
		GROUP BY category
		ORDER BY n DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]int64{}
	for rows.Next() {
		var category string
		var count int64
		if err := rows.Scan(&category, &count); err != nil {
			return nil, err
		}
		out[category] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// loadFailureSamples returns up to maxSamples failing chunks with
// rel_path / category / message. Ordered by category then chunk_id so
// the sample distribution favours coverage across categories rather
// than dumping the first N of one type.
func loadFailureSamples(ctx context.Context, db *sql.DB, maxSamples int) ([]model.FailureSample, error) {
	if maxSamples <= 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT rel_path, COALESCE(NULLIF(error_category, ''), 'unknown') AS category, embedding_error
		FROM chunks
		WHERE deleted = 0 AND embedding_status = 'error'
		ORDER BY category, chunk_id
		LIMIT ?`, maxSamples)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]model.FailureSample, 0, maxSamples)
	for rows.Next() {
		var sample model.FailureSample
		if err := rows.Scan(&sample.RelPath, &sample.Category, &sample.Message); err != nil {
			return nil, err
		}
		out = append(out, sample)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
