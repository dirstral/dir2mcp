package tests

import (
	"bytes"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/pdfutil"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// makePDF renders an n-page PDF (one line of text per page) for tests.
func makePDF(t *testing.T, n int) []byte {
	t.Helper()
	api.DisableConfigDir()
	parts := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		parts = append(parts, `"`+itoa(i)+`":{"content":{"text":[{"value":"page","position":[100,700],"font":{"name":"Helvetica","size":12}}]}}`)
	}
	js := `{"pages":{` + strings.Join(parts, ",") + `}}`
	var buf bytes.Buffer
	if err := api.Create(nil, strings.NewReader(js), &buf, model.NewDefaultConfiguration()); err != nil {
		t.Fatalf("create %d-page pdf: %v", n, err)
	}
	return buf.Bytes()
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

func TestPageCount(t *testing.T) {
	data := makePDF(t, 3)
	n, err := pdfutil.PageCount(data)
	if err != nil {
		t.Fatalf("PageCount: %v", err)
	}
	if n != 3 {
		t.Fatalf("PageCount = %d, want 3", n)
	}
}

func TestExtractPage(t *testing.T) {
	data := makePDF(t, 3)
	page2, err := pdfutil.ExtractPage(data, 2)
	if err != nil {
		t.Fatalf("ExtractPage: %v", err)
	}
	n, err := pdfutil.PageCount(page2)
	if err != nil {
		t.Fatalf("PageCount(extracted): %v", err)
	}
	if n != 1 {
		t.Fatalf("extracted page count = %d, want 1", n)
	}
	if _, err := pdfutil.ExtractPage(data, 0); err == nil {
		t.Fatal("ExtractPage(0) must error")
	}
}
