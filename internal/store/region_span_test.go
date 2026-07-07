package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
)

// decodeRegion round-trips the extra_json a region span flattens to so a test
// can assert the normalized, persisted shape.
func decodeRegion(t *testing.T, extraJSON string) model.RegionSpan {
	t.Helper()
	var r model.RegionSpan
	if err := json.Unmarshal([]byte(extraJSON), &r); err != nil {
		t.Fatalf("decode region extra_json %q: %v", extraJSON, err)
	}
	return r
}

// TestRegionSpanToRow_CoordOriginNormalized proves coord_origin is constrained
// to the §5.4 enum at the persistence boundary: BOTTOMLEFT survives, an
// out-of-enum value is clamped to TOPLEFT, and empty defaults to TOPLEFT.
func TestRegionSpanToRow_CoordOriginNormalized(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bottomleft preserved", "BOTTOMLEFT", "BOTTOMLEFT"},
		{"lowercase bottomleft", "bottomleft", "BOTTOMLEFT"},
		{"empty defaults topleft", "", "TOPLEFT"},
		{"unknown clamped topleft", "CENTER", "TOPLEFT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &model.RegionSpan{
				StartPage: 2, EndPage: 2,
				BBox: &model.BBox{Page: 2, L: 1, T: 2, R: 3, B: 4, CoordOrigin: tc.in},
			}
			kind, start, end, extra, err := regionSpanToRow(r)
			if err != nil {
				t.Fatalf("regionSpanToRow err = %v", err)
			}
			if kind != "region" || start != 2 || end != 2 {
				t.Fatalf("kind/start/end = %q/%d/%d, want region/2/2", kind, start, end)
			}
			if got := decodeRegion(t, extra).BBox.CoordOrigin; got != tc.want {
				t.Fatalf("coord_origin = %q, want %q", got, tc.want)
			}
			// The caller's span must not be mutated by normalization.
			if r.BBox.CoordOrigin != tc.in {
				t.Fatalf("caller bbox mutated: coord_origin = %q, want %q", r.BBox.CoordOrigin, tc.in)
			}
		})
	}
}

// TestRegionSpanToRow_LabelNormalized proves the stored label is collapsed to
// the §5.4 eight-value enum: docling's "title" → section_header, an unknown →
// paragraph, an in-enum value is preserved, and an empty label stays empty.
func TestRegionSpanToRow_LabelNormalized(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"title to section_header", "title", "section_header"},
		{"unknown to paragraph", "footnote", "paragraph"},
		{"in-enum preserved", "table", "table"},
		{"empty stays empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &model.RegionSpan{
				StartPage: 1, EndPage: 1,
				BBox:  &model.BBox{Page: 1, CoordOrigin: "TOPLEFT"},
				Label: tc.in,
			}
			_, _, _, extra, err := regionSpanToRow(r)
			if err != nil {
				t.Fatalf("regionSpanToRow err = %v", err)
			}
			if got := decodeRegion(t, extra).Label; got != tc.want {
				t.Fatalf("label = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRegionSpanToRow_BBoxPageInvariant proves the §5.4 MUST — start_page ≤
// bbox.page ≤ end_page — is validated (hard reject), not clamped.
func TestRegionSpanToRow_BBoxPageInvariant(t *testing.T) {
	cases := []struct {
		name    string
		bbox    int
		start   int
		end     int
		wantErr bool
	}{
		{"in range single", 3, 3, 3, false},
		{"in range multi", 4, 2, 6, false},
		{"below start", 1, 2, 6, true},
		{"above end", 7, 2, 6, true},
		{"zero page below start", 0, 1, 3, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &model.RegionSpan{
				StartPage: tc.start, EndPage: tc.end,
				BBox: &model.BBox{Page: tc.bbox, CoordOrigin: "TOPLEFT"},
			}
			_, _, _, _, err := regionSpanToRow(r)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for bbox.page %d outside [%d,%d], got nil", tc.bbox, tc.start, tc.end)
				}
				if !strings.Contains(err.Error(), "bbox.page") {
					t.Fatalf("error %q does not mention bbox.page", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for bbox.page %d in [%d,%d]: %v", tc.bbox, tc.start, tc.end, err)
			}
		})
	}
}

// TestCheckIndexFormatVersion_MatchAndMismatch proves the §14.3 gate opens a
// current (v1) corpus without a false positive, but a corpus whose persisted
// index_format_version was bumped to an incompatible value is refused with the
// canonical INDEX_VERSION_MISMATCH code and retryable=false.
func TestCheckIndexFormatVersion_MatchAndMismatch(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "meta.sqlite")

	// Fresh v1 store opens clean (no false positive).
	st := NewSQLiteStore(path)
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init fresh store: %v", err)
	}
	// Bump the persisted index format to an incompatible value, then reopen.
	if err := st.SetSetting(ctx, "index_format_version", "2"); err != nil {
		t.Fatalf("bump index_format_version: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reopened := NewSQLiteStore(path)
	err := reopened.Init(ctx)
	if err == nil {
		_ = reopened.Close()
		t.Fatal("expected INDEX_VERSION_MISMATCH on reopen, got nil")
	}
	var mismatch *IndexVersionMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("error is not *IndexVersionMismatchError: %v", err)
	}
	if mismatch.Code() != protocol.ErrorCodeIndexVersionMismatch {
		t.Fatalf("code = %q, want %q", mismatch.Code(), protocol.ErrorCodeIndexVersionMismatch)
	}
	if mismatch.Retryable() {
		t.Fatal("index-version mismatch must not be retryable")
	}
	if mismatch.Persisted != "2" || mismatch.Expected != indexFormatVersion {
		t.Fatalf("persisted/expected = %q/%q, want 2/%q", mismatch.Persisted, mismatch.Expected, indexFormatVersion)
	}
}
