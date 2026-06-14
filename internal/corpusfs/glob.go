package corpusfs

import (
	"path"
	"path/filepath"
	"strings"
)

// matchPathExclude reports whether relPath matches the glob, supporting the `**`
// recursive wildcard. It mirrors ingest.matchPathExclude so the gitignore walker
// moved into this package keeps identical matching semantics.
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
