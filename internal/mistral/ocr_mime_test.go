package mistral

import "testing"

// TestOCRMIMEType_Unchanged pins the exact ext→MIME mapping the flat OCR path
// accepts. It is the byte-identical guard for #395 Stage 1: consolidating the
// scattered format allowlists must not change which extensions Mistral OCR reads
// or the MIME value it sends upstream.
func TestOCRMIMEType_Unchanged(t *testing.T) {
	supported := map[string]string{
		".pdf":  "application/pdf",
		".png":  "image/png",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".webp": "image/webp",
	}
	for ext, wantMIME := range supported {
		gotMIME, ok := ocrMIMEType(ext)
		if !ok {
			t.Errorf("ocrMIMEType(%q) reported unsupported, want %q", ext, wantMIME)
			continue
		}
		if gotMIME != wantMIME {
			t.Errorf("ocrMIMEType(%q) = %q, want %q", ext, gotMIME, wantMIME)
		}
		if !SupportsOCRExt(ext) {
			t.Errorf("SupportsOCRExt(%q) = false, want true", ext)
		}
	}
	// Formats routed to OCR but not readable by it (#394 defect 3) must stay
	// rejected so they are skipped with a diagnostic, never sent upstream.
	for _, ext := range []string{".docx", ".tiff", ".bmp", ".odt", ".rtf", ".doc", ".gif", ".svg", ".html", ".txt", ""} {
		if mime, ok := ocrMIMEType(ext); ok {
			t.Errorf("ocrMIMEType(%q) = (%q, true), want unsupported", ext, mime)
		}
		if SupportsOCRExt(ext) {
			t.Errorf("SupportsOCRExt(%q) = true, want false", ext)
		}
	}
}
