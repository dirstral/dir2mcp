package tests

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/secretmatch"
)

const jwtPattern = `(?i)(?:authorization\s*[:=]\s*bearer\s+|(?:access|id|refresh)_token\s*[:=]\s*)[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`
const bearerPattern = `(?i)token\s*[:=]\s*[A-Za-z0-9_.-]{20,}`

func readCorpus(t *testing.T, filePath string) []string {
	t.Helper()

	file, err := os.Open(filePath)
	if err != nil {
		t.Fatalf("failed to open corpus %s: %v", filePath, err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			t.Errorf("failed to close corpus %s: %v", filePath, closeErr)
		}
	}()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("failed scanning corpus %s: %v", filePath, err)
	}
	return lines
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("failed to locate repository root (go.mod)")
		}
		dir = parent
	}
}

func TestJWTSecretPattern_Corpus(t *testing.T) {
	re, err := regexp.Compile(jwtPattern)
	if err != nil {
		t.Fatalf("invalid regex: %v", err)
	}

	root := repoRoot(t)
	positive := readCorpus(t, filepath.Join(root, "tests", "fixtures", "secret_patterns", "jwt_positive.txt"))
	negative := readCorpus(t, filepath.Join(root, "tests", "fixtures", "secret_patterns", "jwt_negative.txt"))

	positiveMatches := 0
	for _, sample := range positive {
		if re.MatchString(sample) {
			positiveMatches++
		} else {
			t.Errorf("expected positive sample to match: %q", sample)
		}
	}

	falsePositives := 0
	for _, sample := range negative {
		if re.MatchString(sample) {
			falsePositives++
			t.Errorf("expected negative sample not to match: %q", sample)
		}
	}

	t.Logf("jwt regex corpus stats: positives=%d/%d matched, false_positives=%d/%d", positiveMatches, len(positive), falsePositives, len(negative))
}

// TestBearerSecretPattern_Corpus applies the generic bearer-token rule with the
// default allow list (SPEC §7.2), the way the product applies it.
func TestBearerSecretPattern_Corpus(t *testing.T) {
	re, err := secretmatch.Compile([]string{bearerPattern}, config.DefaultSecretAllowPatterns())
	if err != nil {
		t.Fatalf("invalid regex: %v", err)
	}

	root := repoRoot(t)
	positive := readCorpus(t, filepath.Join(root, "tests", "fixtures", "secret_patterns", "bearer_positive.txt"))
	negative := readCorpus(t, filepath.Join(root, "tests", "fixtures", "secret_patterns", "bearer_negative.txt"))

	positiveMatches := 0
	for _, sample := range positive {
		if re.MatchString(sample) {
			positiveMatches++
		} else {
			t.Errorf("expected positive sample to match: %q", sample)
		}
	}

	falsePositives := 0
	for _, sample := range negative {
		if re.MatchString(sample) {
			falsePositives++
			t.Errorf("expected negative sample not to match: %q", sample)
		}
	}

	t.Logf("bearer regex corpus stats: positives=%d/%d matched, false_positives=%d/%d", positiveMatches, len(positive), falsePositives, len(negative))
}

func parseSecretPatternsFromSpec(t *testing.T) []string {
	t.Helper()
	patterns := parseSpecYAMLList(t, "secret_patterns:")
	if len(patterns) == 0 {
		t.Fatal("did not find secret_patterns entries in SPEC.md")
	}
	return patterns
}

// parseSpecYAMLList returns the items of the first YAML list under key in
// SPEC.md (the §16.2 config template). The list ends at the first line that is
// not an item or a comment, so a sibling key never leaks into it.
func parseSpecYAMLList(t *testing.T, key string) []string {
	t.Helper()

	root := repoRoot(t)
	specPath := filepath.Join(root, "dirstral-spec", "docs", "SPEC.md")
	file, err := os.Open(specPath)
	if err != nil {
		t.Fatalf("failed to open dirstral-spec/docs/SPEC.md (run: git submodule update --init): %v", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			t.Errorf("failed to close SPEC.md: %v", closeErr)
		}
	}()

	patterns, err := scanSpecYAMLList(t, file, key)
	if err != nil {
		t.Fatalf("failed scanning SPEC.md: %v", err)
	}
	return patterns
}

func scanSpecYAMLList(t *testing.T, file *os.File, key string) ([]string, error) {
	t.Helper()
	var patterns []string
	inside := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		trimmed := strings.TrimSpace(scanner.Text())
		if trimmed == key {
			inside = true
			continue
		}
		if !inside {
			continue
		}
		if strings.HasPrefix(trimmed, "- ") {
			patterns = append(patterns, parseSecretPatternListItem(t, trimmed))
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		break
	}
	return patterns, scanner.Err()
}

func parseSecretPatternListItem(t *testing.T, trimmed string) string {
	t.Helper()
	value := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
	if strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") && len(value) >= 2 {
		inner := strings.TrimSuffix(strings.TrimPrefix(value, "'"), "'")
		return strings.ReplaceAll(inner, "''", "'")
	}
	if strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"") {
		unquoted, err := strconv.Unquote(value)
		if err != nil {
			t.Fatalf("failed to unquote pattern %q: %v", value, err)
		}
		return unquoted
	}
	return value
}

func TestSpecYAMLSecretPatterns_Compile_JWTAndBearer(t *testing.T) {
	patterns := parseSecretPatternsFromSpec(t)

	var jwtFromSpec string
	var bearerFromSpec string

	for _, pattern := range patterns {
		if strings.Contains(pattern, "authorization") && strings.Contains(pattern, "refresh") {
			jwtFromSpec = pattern
		}
		if strings.Contains(pattern, "token\\s*[:=]") && !strings.Contains(pattern, "authorization") {
			bearerFromSpec = pattern
		}
	}

	if jwtFromSpec == "" {
		t.Fatal("JWT pattern not found in SPEC secret_patterns")
	}
	if bearerFromSpec == "" {
		t.Fatal("bearer token pattern not found in SPEC secret_patterns")
	}

	if _, err := regexp.Compile(jwtFromSpec); err != nil {
		t.Fatalf("JWT pattern from SPEC failed to compile: %v", err)
	}
	if _, err := regexp.Compile(bearerFromSpec); err != nil {
		t.Fatalf("bearer pattern from SPEC failed to compile: %v", err)
	}

	// Ensure our local constants stay in sync with the SPEC patterns
	// Compare compiled string representations to avoid differences in
	// escaping or formatting causing false negatives.
	if regexp.MustCompile(jwtPattern).String() != regexp.MustCompile(jwtFromSpec).String() {
		t.Fatalf("jwtPattern constant and SPEC pattern differ\nlocal: %s\nSPEC : %s",
			jwtPattern, jwtFromSpec)
	}
	if regexp.MustCompile(bearerPattern).String() != regexp.MustCompile(bearerFromSpec).String() {
		t.Fatalf("bearerPattern constant and SPEC pattern differ\nlocal: %s\nSPEC : %s",
			bearerPattern, bearerFromSpec)
	}
}

// TestSpecYAMLSecretAllowPatterns_MatchDefaults pins the shipped allow list to
// the §16.2 template of the pinned spec.
func TestSpecYAMLSecretAllowPatterns_MatchDefaults(t *testing.T) {
	fromSpec := parseSpecYAMLList(t, "secret_allow_patterns:")
	if len(fromSpec) == 0 {
		t.Skip("the pinned spec has no secret_allow_patterns yet (re-pin after dirstral-spec#144 merges)")
	}
	local := config.DefaultSecretAllowPatterns()
	if strings.Join(local, "\n") != strings.Join(fromSpec, "\n") {
		t.Fatalf("default secret_allow_patterns differ from SPEC §16.2\nlocal: %q\nSPEC : %q", local, fromSpec)
	}
}
