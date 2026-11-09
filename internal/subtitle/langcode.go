package subtitle

import (
	"strings"

	"golang.org/x/text/language"
)

// LanguageCode renders a language tag under a code scheme: "iso639_2" gives the
// ISO 639-2 three-letter code of the tag's base language ("ru" -> "rus",
// "en-US" -> "eng"); any other scheme ("bcp47", empty) returns the tag as
// given. A tag with no known base language is returned unchanged.
func LanguageCode(tag, scheme string) string {
	if !strings.EqualFold(strings.TrimSpace(scheme), "iso639_2") {
		return tag
	}
	trimmed := strings.TrimSpace(tag)
	if trimmed == "" {
		return tag
	}
	base, err := language.ParseBase(strings.SplitN(trimmed, "-", 2)[0])
	if err != nil {
		return tag
	}
	return base.ISO3()
}
