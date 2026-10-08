package ingest

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/dirstral/dir2mcp/internal/model"
)

// maxExtractLogReasonRunes bounds the reason in an extraction-failure log line.
// The document row keeps the full message. The log line only has to tell the
// operator which document failed and why, in one short line.
const maxExtractLogReasonRunes = 300

// loggedExtractError marks an extraction error that already has its log line
// (#1117). Its message is the message of the wrapped error, so the document row
// does not change. A caller that logs per-document errors checks
// extractFailureLogged and does not log the same failure a second time.
type loggedExtractError struct{ err error }

func (e *loggedExtractError) Error() string { return e.err.Error() }

func (e *loggedExtractError) Unwrap() error { return e.err }

// extractFailureLogged reports whether err already has its extraction-failure
// log line.
func extractFailureLogged(err error) bool {
	var logged *loggedExtractError
	return errors.As(err, &logged)
}

// logExtractFailure writes one log line for an extraction error that the
// caller records on the document row, and returns err marked as logged (#1117).
// Before, the error went only to the row and the error counter, so an operator
// who watched the daemon log saw indexing continue and never learned which
// document failed, or why.
//
// It logs two kinds of error:
//   - a docling timeout: the line names the document and the limit, so the
//     operator knows which setting to raise;
//   - an extraction provider failure (ErrOCRProviderFailure: docling,
//     docling-serve, Mistral OCR, pandoc or a custom extractor): the line names
//     the engine, the document and a short reason.
//
// Other errors (store or cache writes) pass through unchanged. A cancelled
// caller context (shutdown) logs nothing, because that failure is not a fault
// of the document. The line carries the relative path, never document text,
// and the reason goes through the credential redactors first.
func (s *Service) logExtractFailure(ctx context.Context, doc model.Document, engine string, err error) error {
	if err == nil || ctx.Err() != nil || extractFailureLogged(err) {
		return err
	}
	var timeout *doclingTimeoutError
	switch {
	case errors.As(err, &timeout):
		s.getLogger().Printf("docling: timed out on %s after %s (ingest.docling.timeout_sec=%s); the document is recorded as failed",
			doc.RelPath, timeout.limit, strconv.FormatFloat(timeout.limit.Seconds(), 'f', -1, 64))
	case errors.Is(err, ErrOCRProviderFailure):
		if strings.TrimSpace(engine) == "" {
			engine = "extract"
		}
		s.getLogger().Printf("%s: extraction failed on %s; the document is recorded as failed: %s",
			engine, doc.RelPath, extractLogReason(err))
	default:
		return err
	}
	return &loggedExtractError{err: err}
}

// extractLogReason returns a short, single-line form of err for a log line: the
// credential redactors run first, then the text is cut at the first line break
// and at maxExtractLogReasonRunes. An extractor can put a long stderr in its
// error, and the log line must stay one line.
func extractLogReason(err error) string {
	msg := RedactHighConfidenceCredentials(err.Error())
	if i := strings.IndexAny(msg, "\r\n"); i >= 0 {
		msg = msg[:i]
	}
	msg = strings.TrimSpace(msg)
	if r := []rune(msg); len(r) > maxExtractLogReasonRunes {
		msg = string(r[:maxExtractLogReasonRunes]) + "..."
	}
	return msg
}
