package relpath

import (
	"path"
	"strings"
)

// IsHidden reports whether a corpus-relative path is hidden.
//
// A path is hidden when ANY of its segments starts with a dot. `docs/.env`,
// `docs/.git/config` and `.claude/settings.json` are all hidden;
// `docs/v1.2/file.md` is not, because a dot inside a name is an ordinary
// character.
//
// This is the one rule for `list_files include_hidden=false` (#693). The old
// rule looked at the FIRST segment only, so a dotfile below a visible directory
// was listed as an ordinary file and the flag's meaning changed with directory
// depth.
//
// The input is slash-separated, as every corpus rel_path is (see Normalize,
// which rejects a backslash). The path is cleaned first, so `./docs/.env` and
// `docs/x/../.env` give the same answer as `docs/.env`. An empty path, `.` and
// `..` name no file, so they are not hidden. A path that still starts with
// `../` after cleaning is hidden: it is not a valid rel_path at all, and a
// listing filter must fail toward hiding.
//
// NotHiddenSQL is the SQL form of this rule. Change the two together.
func IsHidden(rel string) bool {
	cleaned := path.Clean(strings.TrimSpace(rel))
	if cleaned == "." || cleaned == "/" {
		return false
	}
	for _, segment := range strings.Split(cleaned, "/") {
		if segment != "." && strings.HasPrefix(segment, ".") {
			return true
		}
	}
	return false
}

// NotHiddenSQL is IsHidden as a SQL predicate over column: it keeps the rows
// IsHidden calls visible, and drops the rest.
//
// A store that pages in SQL cannot call IsHidden per row and still report an
// honest total, so the rule has to exist in both forms. It lives beside IsHidden
// so the pair stays in step; #716 is what a second, distant copy costs.
//
// The two forms agree byte for byte on every stored rel_path, because a stored
// rel_path is already cleaned and slash-separated: it has no `./` prefix, no
// `.` or `..` segment, and no trailing slash. So "some segment starts with a
// dot" is exactly "the path starts with a dot, or it contains `/.`". Neither
// `.` nor `/` is a LIKE metacharacter, so no ESCAPE clause is needed.
//
// column must be a trusted identifier from the caller's own SQL, never user
// input.
func NotHiddenSQL(column string) string {
	return "(" + column + " NOT LIKE '.%' AND " + column + " NOT LIKE '%/.%')"
}
