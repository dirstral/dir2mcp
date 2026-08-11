package ingest

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// secretScanSampleBytes is the head-sample budget for a source whose searchable
// text is NOT its raw bytes: a PDF, an image, an office document, audio, video.
// Scanning a container's compressed or encoded bytes past the head buys nothing,
// because a credential inside such a file only becomes readable text after
// extraction, OCR, or transcription. Those DERIVED texts are scanned in full
// before they are persisted (see Service.screenDerivedSecrets), so the head
// sample is a cheap early-out, not the security boundary.
//
// A source whose raw bytes ARE the searchable text (text, code, markdown, data,
// html) is scanned in FULL instead; see secretScanBytes. #681: the head sample
// used to be the only scan for every source, so a credential past 64 KiB was
// indexed and reachable through `search`.
const secretScanSampleBytes int64 = 64 * 1024

// secretScanBytes returns the bytes of a source document that the ingest secret
// policy scans, given the document's classified doc_type.
//
// The rule is "scan what becomes searchable":
//
//   - a raw-text doc_type (text/code/md/data/html) is chunked and embedded from
//     these very bytes, so every byte is scanned. There is no offset past which
//     a credential stops being indexed, so there must be none past which it
//     stops being scanned.
//   - every other doc_type reaches the index only through a derived text
//     (extraction, OCR, transcription, annotation, recognition, a subtitle
//     sidecar). Its raw bytes are a container, so only the head sample is
//     scanned here and the derived text carries the full scan.
//
// The doc_type split is what keeps the cost proportionate: it never runs the
// regex set over the bytes of a 10 MiB video, which no pattern could usefully
// match anyway.
//
// One case sits between the two: a binary payload that classified into a
// text-oriented doc_type, such as a .parquet reaching "data". #398 already
// refuses to put such content on the raw-text path, so it is never chunked and
// never indexed. It therefore takes the head sample too: a full scan of it would
// be the whole cost of the change with none of the benefit.
func secretScanBytes(docType string, content []byte) []byte {
	if ShouldGenerateRawText(docType) && !looksLikeBinaryContent(content) {
		return content
	}
	return contentSample(content)
}

func compileSecretPatterns(patterns []string) ([]*regexp.Regexp, error) {
	return CompileSecretPatterns(patterns)
}

// CompileSecretPatterns compiles secret-detection regexes from config.
func CompileSecretPatterns(patterns []string) ([]*regexp.Regexp, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		rx, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("compile secret pattern %q: %w", pattern, err)
		}
		compiled = append(compiled, rx)
	}
	return compiled, nil
}

func hasSecretMatch(sample []byte, patterns []*regexp.Regexp) bool {
	return HasSecretMatch(sample, patterns)
}

// HasSecretMatch reports whether any compiled secret regex matches sample.
func HasSecretMatch(sample []byte, patterns []*regexp.Regexp) bool {
	for _, rx := range patterns {
		if rx.Match(sample) {
			return true
		}
	}
	return false
}

// RedactSecretsInMessage replaces any substring matching one of the
// configured secret patterns with the literal "[REDACTED]". Used before
// persisting upstream error text to documents.error_message (and thus
// to the support bundle) so that a provider error that quotes back the
// triggering payload — e.g. a misconfigured Authorization header or an
// echoed API key — cannot leak through the diagnostic surface.
func RedactSecretsInMessage(msg string, patterns []*regexp.Regexp) string {
	if msg == "" {
		return msg
	}
	out := msg
	for _, rx := range patterns {
		if rx == nil {
			continue
		}
		out = rx.ReplaceAllString(out, "[REDACTED]")
	}
	return out
}

// highConfidenceCredentialRedactors is the shared safety net of high-confidence
// credential SHAPES — unambiguous token forms scrubbed from a failure message
// before it is shown on any diagnostic surface. It is the common base used by
// both RedactHighConfidenceCredentials (MCP recent_failures) and
// RedactCredentialsForDisplay (CLI `status`), so the two surfaces cannot drift.
// It deliberately contains only unambiguous shapes — never a generic
// `keyword: value` form — so a redaction here never hides the actionable part of
// an error (SPEC §15.6 recent_failures actionability).
var highConfidenceCredentialRedactors = []*regexp.Regexp{
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-+/=]{20,}`),                             // Bearer token
	regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),                                    // AWS access key (long-term / temporary)
	regexp.MustCompile(`(?i)\b(?:sk|pk|rk)[-_][A-Za-z0-9_\-]{16,}`),                        // Stripe / OpenAI (sk-proj-…) / Anthropic-style key
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9_]{20,}`),                                    // GitHub PAT / OAuth
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`),                                   // Slack
	regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-.]{10,}\.[A-Za-z0-9_\-]{5,}`), // JWT
}

// displayKeyValueRedactor additionally scrubs generic `keyword: value` /
// `keyword=value` credential assignments. It is applied ONLY on the CLI display
// surface (RedactCredentialsForDisplay), where redacting `password=hunter2` in
// the operator's terminal is worth the small risk of also masking a benign
// `token: expired` — a tradeoff the MCP recent_failures surface deliberately
// does not make (it feeds a client that needs the actionable failure text).
var displayKeyValueRedactor = regexp.MustCompile(`(?i)(authorization|api[_-]?key|token|secret|password|passwd)\s*[:=]\s*\S+`)

func applyRedactors(msg string, redactors []*regexp.Regexp) string {
	if msg == "" {
		return msg
	}
	for _, rx := range redactors {
		msg = rx.ReplaceAllString(msg, "[REDACTED]")
	}
	return msg
}

// RedactHighConfidenceCredentials replaces any substring matching a
// high-confidence credential shape with "[REDACTED]". Used by the MCP
// recent_failures surface (SPEC §15.6 "error_message MUST NOT contain secrets").
// Returns msg unchanged when nothing matches.
func RedactHighConfidenceCredentials(msg string) string {
	return applyRedactors(msg, highConfidenceCredentialRedactors)
}

// RedactCredentialsForDisplay applies the high-confidence redactors plus the
// generic `keyword: value` redactor. Used by the CLI `status` coverage report,
// a human-facing surface that can afford more aggressive scrubbing than the
// MCP tool output. Returns msg unchanged when nothing matches.
func RedactCredentialsForDisplay(msg string) string {
	msg = applyRedactors(msg, highConfidenceCredentialRedactors)
	return applyRedactors(msg, []*regexp.Regexp{displayKeyValueRedactor})
}

func matchesAnyPathExclude(relPath string, globs []string) bool {
	return MatchesAnyPathExclude(relPath, globs)
}

// MatchesAnyPathExclude reports whether relPath matches any configured glob.
func MatchesAnyPathExclude(relPath string, globs []string) bool {
	normalizedPath := normalizeForGlob(relPath)
	if normalizedPath == "" {
		return false
	}
	for _, glob := range globs {
		if matchPathExclude(glob, normalizedPath) {
			return true
		}
	}
	return false
}

func matchPathExclude(glob, relPath string) bool {
	pattern := normalizeForGlob(glob)
	if pattern == "" {
		return false
	}
	if strings.HasSuffix(pattern, "/") {
		pattern += "**"
	}
	return matchGlobSegments(strings.Split(pattern, "/"), strings.Split(relPath, "/"))
}

// MatchesGlobPattern checks whether filePath matches pattern.
func MatchesGlobPattern(filePath, pattern string) bool {
	return matchPathExclude(pattern, filePath)
}

func matchGlobSegments(pattern, value []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			for len(pattern) > 1 && pattern[1] == "**" {
				pattern = pattern[1:]
			}
			if len(pattern) == 1 {
				return true
			}
			for i := 0; i <= len(value); i++ {
				if matchGlobSegments(pattern[1:], value[i:]) {
					return true
				}
			}
			return false
		}

		if len(value) == 0 {
			return false
		}

		ok, err := path.Match(pattern[0], value[0])
		if err != nil || !ok {
			return false
		}
		pattern = pattern[1:]
		value = value[1:]
	}
	return len(value) == 0
}

func normalizeForGlob(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	raw = filepath.ToSlash(raw)
	raw = strings.TrimPrefix(raw, "./")
	raw = strings.TrimPrefix(raw, "/")
	return strings.TrimSpace(raw)
}
