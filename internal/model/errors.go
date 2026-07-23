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

	// ErrRelatedSourceNotFound is returned by RelatedSearcher.Related when the
	// seed chunk_id / rel_path resolves to no indexed segment (SPEC §15.12): the
	// source could not be located, so the tool layer surfaces INVALID_FIELD rather
	// than an empty result.
	ErrRelatedSourceNotFound = errors.New("related source segment not found")

	// ErrRelatedNotSupported is returned by RelatedSearcher.Related when the
	// configured store cannot resolve seed segments, so dir2mcp_related cannot be
	// served.
	ErrRelatedNotSupported = errors.New("related retrieval not supported by this store")

	// ErrMediaNoText indicates a multimodal media-only document (SPEC 8.1.7):
	// embedded directly under model.embed.multimodal=replace with no text
	// representation. This is a permanent condition — unlike ErrOCRNotReady,
	// retrying will not produce text — so open_file MUST surface it as the
	// non-retryable MEDIA_NO_TEXT and never fall back to raw bytes.
	ErrMediaNoText = errors.New("media has no text representation")
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
