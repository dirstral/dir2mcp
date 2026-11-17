package ingest

import (
	"bufio"
	"bytes"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// priorityList is the parsed ingest.priority_list (SPEC §7.1): each entry is a
// corpus-relative file or directory path, ranked by the line it first appears
// on.
type priorityList struct {
	rank    map[string]int
	entries int
	bad     int
	badLine string
}

// parsePriorityList reads one corpus-relative path per line. Blank lines and
// lines that start with "#" are skipped. An absolute entry or one with a ".."
// segment is counted as bad and dropped. A repeated entry keeps its first rank.
func parsePriorityList(data []byte) priorityList {
	pl := priorityList{rank: map[string]int{}}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		entry, ok := cleanPriorityEntry(line)
		if !ok {
			pl.bad++
			if pl.badLine == "" {
				pl.badLine = line
			}
			continue
		}
		if _, seen := pl.rank[entry]; !seen {
			pl.rank[entry] = pl.entries
		}
		pl.entries++
	}
	return pl
}

// cleanPriorityEntry normalizes one list entry to the form of a discovered
// RelPath. It rejects an absolute path and any ".." segment: an entry names a
// path inside the corpus only.
func cleanPriorityEntry(line string) (string, bool) {
	slashed := filepath.ToSlash(line)
	if strings.HasPrefix(slashed, "/") || filepath.IsAbs(line) {
		return "", false
	}
	for _, seg := range strings.Split(slashed, "/") {
		if seg == ".." {
			return "", false
		}
	}
	cleaned := path.Clean(slashed)
	if cleaned == "." {
		return "", false
	}
	return cleaned, true
}

// rankOf is the best (lowest) rank of an entry that covers relPath: the file
// itself or any directory above it. ok is false when no entry covers it.
func (pl priorityList) rankOf(relPath string) (int, string, bool) {
	best, bestEntry, found := math.MaxInt, "", false
	consider := func(key string) {
		if r, ok := pl.rank[key]; ok && r < best {
			best, bestEntry, found = r, key, true
		}
	}
	consider(relPath)
	for i := 0; i < len(relPath); i++ {
		if relPath[i] == '/' {
			consider(relPath[:i])
		}
	}
	return best, bestEntry, found
}

// orderForProcessing returns the discovered files in the order a pass
// processes them (SPEC §7.1, §8.6.11). Without ingest.priority_list it is the
// discovered (relative-path) order. With it, the files a list entry covers come
// first, by the rank of the first entry that covers them and then by relative
// path, followed by every other file in relative-path order. The set of files
// never changes, and a list that cannot be read is a warning, not an error.
func (s *Service) orderForProcessing(discovered []DiscoveredFile) []DiscoveredFile {
	listPath := strings.TrimSpace(s.cfg.IngestPriorityList)
	if listPath == "" {
		return discovered
	}
	if !filepath.IsAbs(listPath) {
		listPath = filepath.Join(s.cfg.RootDir, listPath)
	}
	logger := s.getLogger()
	data, err := os.ReadFile(listPath)
	if err != nil {
		logger.Printf("ingest.priority_list: cannot read %s (%v); processing in relative-path order", listPath, err)
		return discovered
	}
	pl := parsePriorityList(data)
	if pl.bad > 0 {
		logger.Printf("ingest.priority_list: ignored %d entries that are absolute or contain '..' (first: %q)", pl.bad, pl.badLine)
	}

	ranks := make([]int, len(discovered))
	used := make(map[string]struct{}, len(pl.rank))
	listed := 0
	for i, f := range discovered {
		r, entry, ok := pl.rankOf(filepath.ToSlash(f.RelPath))
		ranks[i] = r
		if ok {
			listed++
			used[entry] = struct{}{}
		}
	}
	idx := make([]int, len(discovered))
	for i := range idx {
		idx[i] = i
	}
	// Stable on the discovered (relative-path) order, so ties and the unlisted
	// files keep relative-path order.
	sort.SliceStable(idx, func(a, b int) bool { return ranks[idx[a]] < ranks[idx[b]] })
	out := make([]DiscoveredFile, len(discovered))
	for i, j := range idx {
		out[i] = discovered[j]
	}
	logger.Printf("ingest.priority_list: %d of %d files listed first (%d entries; %d placed no file)",
		listed, len(discovered), len(pl.rank), len(pl.rank)-len(used))
	return out
}
