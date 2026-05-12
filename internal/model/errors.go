package model

import "errors"

var (
	// ErrNotImplemented marks skeleton methods that still need subsystem work.
	ErrNotImplemented = errors.New("not implemented")

	// ErrNotFound is used by store implementations to indicate a requested
	// record (document, representation, chunk, etc.) does not exist.  Higher
	// layers may treat this the same as sql.ErrNoRows.
	ErrNotFound = errors.New("not found")

	// ErrIndexNotReady is returned by a Retriever when the index exists but
	// has not yet finished building or loading.
	ErrIndexNotReady = errors.New("index not ready")

	// ErrIndexNotConfigured is returned by a Retriever when no index has been
	// configured at all (e.g. the caller never provided one).
	ErrIndexNotConfigured = errors.New("index not configured")

	// ErrPathOutsideRoot indicates a requested path resolves outside configured root.
	ErrPathOutsideRoot = errors.New("path outside root")

	// ErrForbidden indicates access was denied by security policy.
	ErrForbidden = errors.New("forbidden")

	// ErrDocTypeUnsupported indicates the requested span/doc mode isn't supported.
	ErrDocTypeUnsupported = errors.New("doc type unsupported")

	// ErrOCRNotReady indicates that an OCR/transcript representation has not
	// yet been computed for a binary document (e.g. PDF, audio). Callers
	// should retry once ingestion completes rather than fall back to raw bytes.
	ErrOCRNotReady = errors.New("ocr not ready")
)

type ProviderError struct {
	Code       string
	Message    string
	Retryable  bool
	StatusCode int
	Cause      error
}

func (e *ProviderError) Error() string {
	if e == nil {
		return "<nil ProviderError>"
	}
	if e.Code == "" && e.Message == "" {
		return "<empty ProviderError>"
	}
	if e.Code == "" {
		return e.Message
	}
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

func (e *ProviderError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}
