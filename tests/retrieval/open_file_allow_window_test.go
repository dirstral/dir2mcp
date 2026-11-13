package tests

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/internal/secretmatch"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §7.2 / §15.4 (spec 0.93.0): open_file checks the WHOLE source against
// the secret policy before it returns any byte, so its decision is exactly the
// decision secretmatch.Matcher.Match makes on the whole text. The cases below
// broke earlier designs that judged the source in read windows.

func openFileService(t *testing.T, root string, secret, allow []string) *retrieval.Service {
	t.Helper()
	svc := newBoundedService(root)
	if err := svc.SetSecretPolicy(secret, allow); err != nil {
		t.Fatalf("SetSecretPolicy: %v", err)
	}
	return svc
}

func writeRootFile(t *testing.T, root, relPath string, body []byte) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(abs, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestOpenFile_WholeSourceDecisionEqualsMatch(t *testing.T) {
	tok := "to" + "ken"
	cases := []struct {
		name   string
		allow  []string
		body   string
		refuse bool
	}{
		// An allow match ends at a 64 KiB read boundary, but the text runs on.
		{"anchored allow, run goes on", []string{`^` + tok + `=A+$`}, tok + "=" + strings.Repeat("A", 100000) + "!", true},
		{"anchored allow, whole source is one allow match", []string{`^` + tok + `=A+$`}, tok + "=" + strings.Repeat("A", 100000), false},
		// The key and its value are 65,531 spaces apart: \s* is unbounded.
		{"key far before its value", nil, tok + strings.Repeat(" ", 65531) + "=" + strings.Repeat("A", 24), true},
		// An unanchored allow match at a cut must not make an anchored one
		// match there.
		{"anchored allow far from the start", []string{`prefix`, `^prefix\s*` + tok + `=[A-Z]+!`},
			strings.Repeat("x", 61440) + "prefix" + strings.Repeat(" ", 4090) + tok + "=" + strings.Repeat("A", 24) + "!", true},
		{"anchored allow at the real start", []string{`prefix`, `^prefix\s*` + tok + `=[A-Z]+!`},
			"prefix" + strings.Repeat(" ", 4090) + tok + "=" + strings.Repeat("A", 24) + "!", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := secretmatch.Compile([]string{secretmatch.GenericTokenPattern}, tc.allow)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if got := m.MatchString(tc.body); got != tc.refuse {
				t.Fatalf("whole-text Match = %v, the case expects %v", got, tc.refuse)
			}
			root := testutil.TempDir(t)
			writeRootFile(t, root, "docs/case.txt", []byte(tc.body))
			svc := openFileService(t, root, []string{secretmatch.GenericTokenPattern}, tc.allow)
			_, err = svc.OpenFile(context.Background(), "docs/case.txt", model.Span{}, 20000)
			if tc.refuse && !errors.Is(err, model.ErrForbidden) {
				t.Fatalf("OpenFile err = %v, want ErrForbidden (Match refuses this source)", err)
			}
			if !tc.refuse && err != nil {
				t.Fatalf("OpenFile err = %v, want the head of the source (Match serves it)", err)
			}
		})
	}
}

// TestOpenFile_RandomSourcesEqualMatch compares the open_file decision with
// Match on random sources built from benign links, credentials, long runs and
// long whitespace gaps, for the shipped defaults and two operator allow lists.
func TestOpenFile_RandomSourcesEqualMatch(t *testing.T) {
	tok := "to" + "ken"
	upper := strings.ToUpper(tok)
	cfg := config.Default()
	policies := []struct {
		name          string
		secret, allow []string
	}{
		{"defaults", cfg.SecretPatterns, cfg.SecretAllowPatterns},
		{"anchored", []string{secretmatch.GenericTokenPattern}, []string{`^` + tok + `=A+$`, `prefix`, `^prefix\s*` + tok + `=[A-Z]+!`}},
		{"suffix", []string{secretmatch.GenericTokenPattern}, []string{`feature_` + tok + `=[A-Za-z0-9]+;ok`}},
	}
	pieces := []string{
		"plain prose about the regional budget. ",
		"https://www.youtube.com/redirect?event=video_description&redir_" + tok + "=QUFFLUhqbkJ3T2xYbE5fZ3lQdmZ4MTIz==&q=x ",
		"https://example.org/list?page_" + tok + "=abc?GITHUB_" + upper + "=Xy7Zq9WvXy7Zq9WvXy7Zq9WvXy7Zq9Wv ",
		"GITHUB_" + upper + "=Xy7Zq9WvXy7Zq9WvXy7Zq9WvXy7Zq9Wv\n",
		"feature_" + tok + "=Xy7Zq9WvXy7Zq9WvXy7Zq9WvXy7Zq9;ok ",
		"feature_" + tok + "=Xy7Zq9WvXy7Zq9WvXy7Zq9WvXy7Zq9;no ",
		tok + strings.Repeat(" ", 70000) + "=" + strings.Repeat("A", 24),
		"prefix" + strings.Repeat(" ", 4090) + tok + "=" + strings.Repeat("A", 24) + "!",
		strings.Repeat("x", 61440),
		tok + "=" + strings.Repeat("A", 70000),
		"!",
	}
	rng := rand.New(rand.NewSource(1167))
	root := testutil.TempDir(t)
	services := make([]*retrieval.Service, len(policies))
	matchers := make([]*secretmatch.Matcher, len(policies))
	for i, p := range policies {
		services[i] = openFileService(t, root, p.secret, p.allow)
		m, err := secretmatch.Compile(p.secret, p.allow)
		if err != nil {
			t.Fatalf("compile %s: %v", p.name, err)
		}
		matchers[i] = m
	}
	var refused, served int
	for trial := 0; trial < 60; trial++ {
		var b strings.Builder
		for n := 1 + rng.Intn(5); n > 0; n-- {
			b.WriteString(pieces[rng.Intn(len(pieces))])
		}
		rel := fmt.Sprintf("docs/random-%d.txt", trial)
		writeRootFile(t, root, rel, []byte(b.String()))
		for i, p := range policies {
			want := matchers[i].MatchString(b.String())
			_, err := services[i].OpenFile(context.Background(), rel, model.Span{}, 200)
			got := errors.Is(err, model.ErrForbidden)
			if !got && err != nil {
				t.Fatalf("%s trial %d: OpenFile err = %v", p.name, trial, err)
			}
			if got != want {
				t.Fatalf("%s trial %d: open_file refused=%v, Match=%v", p.name, trial, got, want)
			}
			if want {
				refused++
			} else {
				served++
			}
		}
	}
	if refused == 0 || served == 0 {
		t.Fatalf("random sources are one-sided: %d refused, %d served", refused, served)
	}
}

// TestOpenFile_SourceAboveTheCapIsFileTooLarge pins the bound: ingest never
// content-checked or indexed a file above ingest.max_file_mb, so open_file
// refuses it with FILE_TOO_LARGE and reads no more than cap+1 bytes.
func TestOpenFile_SourceAboveTheCapIsFileTooLarge(t *testing.T) {
	root := testutil.TempDir(t)
	const capBytes = 1 << 20
	body := repeatToSize("remote object line with enough text to be interesting\n", 4<<20)
	fs := &cappedCorpusFS{objects: map[string][]byte{"docs/big.md": []byte(body)}, cap: capBytes + 1}
	svc := newBoundedService(root)
	svc.SetCorpusFS(fs)
	svc.SetMaxFileBytes(capBytes)

	_, err := svc.OpenFile(context.Background(), "docs/big.md", model.Span{}, 20000)
	if !errors.Is(err, corpusfs.ErrObjectTooLarge) {
		t.Fatalf("OpenFile err = %v, want corpusfs.ErrObjectTooLarge", err)
	}
	if fs.bytesRead > capBytes+1 {
		t.Fatalf("read %d bytes; the read must stop at cap+1 (%d)", fs.bytesRead, capBytes+1)
	}

	// A source at the cap is served.
	fs.objects["docs/at-cap.md"] = []byte(body[:capBytes])
	fs.bytesRead = 0
	if _, err := svc.OpenFile(context.Background(), "docs/at-cap.md", model.Span{}, 20000); err != nil {
		t.Fatalf("OpenFile at the cap: %v", err)
	}
}
