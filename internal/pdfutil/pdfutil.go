// Package pdfutil provides minimal, dependency-isolated PDF helpers for
// multimodal embedding (SPEC 8.1.7): counting pages and extracting a single
// page as its own PDF, so each page can be embedded directly with a precise
// page citation. It wraps pdfcpu and keeps that dependency out of the rest of
// the codebase.
package pdfutil

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

func init() {
	// Keep PDF handling hermetic: never read or write a user/CI config
	// directory (pdfcpu falls back to built-in defaults).
	api.DisableConfigDir()
}

// conf returns a fresh default pdfcpu configuration. A non-nil config is
// required: some pdfcpu API functions (e.g. PageCount) dereference it.
func conf() *model.Configuration {
	return model.NewDefaultConfiguration()
}

// PageCount returns the number of pages in the PDF bytes.
func PageCount(data []byte) (int, error) {
	n, err := api.PageCount(bytes.NewReader(data), conf())
	if err != nil {
		return 0, fmt.Errorf("pdf page count: %w", err)
	}
	return n, nil
}

// ExtractPage returns a single-page PDF containing the given 1-based page of
// data, so it can be embedded on its own with a precise page span.
//
// Whole-file input (issue #243, deliberate): this takes the full PDF bytes rather
// than an io.ReadSeeker that an S3 backend could range-GET. A ReadSeeker path was
// evaluated and rejected — pdfcpu's Trim parses the entire cross-reference table
// and re-reads the source many times over (measured reaching 100% of the object
// and ~36x the file size in total reads), so streaming it over range GETs would
// be strictly worse than one whole-object download. The caller reads the whole
// file once and caches it per PDF per batch (see the embedding worker's
// loadPDFPage), which is the right shape for pdfcpu. Audio/video, by contrast, do
// range-read on S3 via ffmpeg-over-HTTP (avutil.ExtractSegmentURL).
func ExtractPage(data []byte, page int) ([]byte, error) {
	if page < 1 {
		return nil, fmt.Errorf("pdf page must be >= 1, got %d", page)
	}
	var buf bytes.Buffer
	if err := api.Trim(bytes.NewReader(data), &buf, []string{strconv.Itoa(page)}, conf()); err != nil {
		return nil, fmt.Errorf("extract pdf page %d: %w", page, err)
	}
	return buf.Bytes(), nil
}
