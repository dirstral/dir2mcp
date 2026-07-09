package model

import "strings"

// Per-language retrieval filter helpers (SPEC §9.5) and representation language
// recording (§5.2/§8.8). The matching contract is intentionally minimal and
// dependency-free: a request matches a recorded representation language when
// their BCP-47 **primary subtags** are equal, compared case-insensitively. We do
// not pull in a full BCP-47 parser because §9.5 only mandates primary-subtag
// matching ("Implementations MAY additionally honor an exact full-tag match but
// MUST AT LEAST honor primary-subtag matching"). Region, script, and other
// subtags never cause a match to be missed when the primary subtags agree.

// LanguagePrimarySubtag returns the lower-cased BCP-47 primary subtag of a
// language tag — the segment before the first '-' or '_' — trimmed of
// surrounding whitespace. It returns "" for an empty/whitespace tag (which the
// caller treats as "unknown language"). It is the canonical key used for both
// recording a representation's effective language and matching it against a
// requested filter tag (§9.5), so the two are always compared on the same axis.
func LanguagePrimarySubtag(tag string) string {
	t := strings.ToLower(strings.TrimSpace(tag))
	if t == "" {
		return ""
	}
	if i := strings.IndexAny(t, "-_"); i >= 0 {
		return t[:i]
	}
	return t
}

// IsValidLanguageTag reports whether tag is a syntactically valid BCP-47
// language tag for the purpose of the per-language retrieval filter (§9.5). The
// check is deliberately lenient — it accepts the common forms an operator or
// client sends (`en`, `EN`, `pt-BR`, `zh-Hant`, `und`) and rejects only clearly
// malformed values — because §9.5 requires that an *unrecognized or malformed*
// tag be reported as INVALID_FIELD, while a syntactically valid tag that simply
// matches nothing is NOT an error.
//
// Validity rule: the tag is one or more '-'-separated subtags, each composed
// solely of ASCII letters/digits, each 1..8 chars, with a non-empty primary
// subtag of letters only (1..8). A trailing/leading/double hyphen, an empty
// subtag, or any non-alphanumeric/non-hyphen rune is invalid. An empty/blank tag
// is invalid (callers strip empties before validating; an explicit empty string
// in the array is a client error).
func IsValidLanguageTag(tag string) bool {
	t := strings.TrimSpace(tag)
	if t == "" {
		return false
	}
	subtags := strings.Split(t, "-")
	for idx, sub := range subtags {
		if len(sub) < 1 || len(sub) > 8 {
			return false
		}
		for _, r := range sub {
			isLetter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
			isDigit := r >= '0' && r <= '9'
			if !isLetter && !isDigit {
				return false
			}
			// The primary subtag (first) must be letters only — a language code is
			// never numeric (e.g. "1" or "123" is not a language).
			if idx == 0 && isDigit {
				return false
			}
		}
	}
	return true
}

// LanguageMatch selects the §9.5 matching mode for the per-language retrieval
// filter. The zero value ("" ⇒ LanguageMatchPrimary) preserves the historical
// default so callers that never set a mode observe no behaviour change.
const (
	// LanguageMatchPrimary is the DEFAULT: BCP-47 primary-subtag matching. A
	// request for `pt` (or `pt-BR`) matches a representation recorded as `pt`,
	// `PT`, or `pt-BR`; region/script/variant subtags never cause a match to be
	// missed when the primary subtags agree (§9.5).
	LanguageMatchPrimary = "primary"
	// LanguageMatchStrict is the opt-in narrowing mode: BCP-47 Basic Filtering
	// (RFC 4647 §3.3.1). Region/script/variant subtags in the request DO narrow
	// the match — `pt-BR` matches `pt-BR`/`pt-BR-…` but not bare `pt` or `pt-PT`
	// (§9.5).
	LanguageMatchStrict = "strict"
)

// NormalizeLanguageMatch maps a requested match mode to its canonical form.
// Absent/empty ⇒ LanguageMatchPrimary (the default, §9.5). It does NOT validate:
// the MCP layer rejects an unrecognized value as INVALID_FIELD before this is
// reached, and any non-"strict" value degrades safely to the recall-preserving
// primary default here.
func NormalizeLanguageMatch(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), LanguageMatchStrict) {
		return LanguageMatchStrict
	}
	return LanguageMatchPrimary
}

// IsValidLanguageMatch reports whether mode is a recognized §9.5 match mode.
// Absent/empty is valid (⇒ the primary default); any other value than
// "primary"/"strict" (case-insensitive) is INVALID_FIELD (§9.5/§14).
func IsValidLanguageMatch(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", LanguageMatchPrimary, LanguageMatchStrict:
		return true
	default:
		return false
	}
}

// LanguageMatchesAny reports whether a representation's recorded effective
// language (recordedTag, §5.2) matches any of the requested filter tags under
// the §9.5 DEFAULT semantics: case-insensitive **primary-subtag** match, logical
// OR across the requested set. It is equivalent to
// LanguageMatchesAnyMode(recordedTag, requested, LanguageMatchPrimary) and is
// retained for callers that only need the default mode.
//
//   - An empty requested set means "no filter" and is handled by the caller, not
//     here; LanguageMatchesAny is only consulted when a filter is active.
//   - A representation with no recorded language (recordedTag == "" ⇒ unknown,
//     §8.8) NEVER matches a specific filter — it returns false here, so it is
//     excluded whenever the filter is non-empty.
//   - Requested tags are assumed already validated (IsValidLanguageTag) by the
//     MCP layer; an entry that reduces to an empty primary subtag matches
//     nothing.
func LanguageMatchesAny(recordedTag string, requested []string) bool {
	recorded := LanguagePrimarySubtag(recordedTag)
	if recorded == "" {
		// Unknown language: never matches a specific filter (§9.5).
		return false
	}
	for _, req := range requested {
		if LanguagePrimarySubtag(req) == recorded {
			return true
		}
	}
	return false
}

// LanguageMatchesAnyMode reports whether a representation's recorded effective
// language matches any requested filter tag under the selected §9.5 mode
// (logical OR across the requested set).
//
//   - LanguageMatchPrimary (default, incl. "" or any non-"strict" value):
//     case-insensitive primary-subtag matching (see LanguageMatchesAny).
//   - LanguageMatchStrict: BCP-47 Basic Filtering (RFC 4647 §3.3.1). A requested
//     tag matches iff the recorded value equals it or extends it with additional
//     subtags (recorded begins with `<request>-`), compared case-insensitively.
//     Region/script/variant subtags in the request thus narrow the match, but
//     only to the precision the caller supplied (a bare primary subtag still
//     matches all its region/script extensions).
//
// In BOTH modes a representation with no recorded language (unknown, §8.8) never
// matches a specific filter.
func LanguageMatchesAnyMode(recordedTag string, requested []string, mode string) bool {
	if NormalizeLanguageMatch(mode) != LanguageMatchStrict {
		return LanguageMatchesAny(recordedTag, requested)
	}
	recorded := strings.ToLower(strings.TrimSpace(recordedTag))
	if recorded == "" {
		// Unknown language: never matches a specific filter (§9.5).
		return false
	}
	for _, req := range requested {
		if languageTagMatchesStrict(req, recorded) {
			return true
		}
	}
	return false
}

// languageTagMatchesStrict implements RFC 4647 §3.3.1 Basic Filtering for a
// single (requested, recorded) pair: the requested tag matches when the
// canonicalized recorded value equals it or extends it with a `-`-delimited
// suffix, compared case-insensitively. recorded is assumed already lower-cased
// and trimmed by the caller; requested is canonicalized here.
func languageTagMatchesStrict(requested, recorded string) bool {
	req := strings.ToLower(strings.TrimSpace(requested))
	if req == "" || recorded == "" {
		return false
	}
	if recorded == req {
		return true
	}
	// Recorded extends the request with additional subtags: the boundary after
	// the request MUST be a subtag separator so `pt-br` never matches `pt-brz`.
	return strings.HasPrefix(recorded, req+"-")
}
