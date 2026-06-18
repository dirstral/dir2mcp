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

// LanguageMatchesAny reports whether a representation's recorded effective
// language (recordedTag, §5.2) matches any of the requested filter tags under
// the §9.5 semantics: case-insensitive **primary-subtag** match, logical OR
// across the requested set.
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
