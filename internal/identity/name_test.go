package identity

import (
	"strings"
	"testing"
)

func TestAutoServerName(t *testing.T) {
	cases := []struct {
		name     string
		rootAbs  string
		wantSlug string
	}{
		{"simple", "/home/user/notes", "notes"},
		{"with spaces", "/home/user/Stas Legal", "stas-legal"},
		{"mixed punctuation", "/var/lib/Foo_Bar.v2!", "foo-bar-v2"},
		{"trailing slash", "/var/lib/foo/", "foo"},
		{"unicode accents collapse to dashes", "/tmp/Café Münster", "caf-m-nster"},
		{"all-symbol basename falls back", "/tmp/!!!", "dir"},
		{"empty basename via root path", "/", "dir"},
		{"long basename gets truncated", "/x/" + strings.Repeat("a", 64), strings.Repeat("a", slugMaxLen)},
		{"long with trailing dash after truncation", "/x/" + strings.Repeat("a", slugMaxLen-1) + "-bbbbbb", strings.Repeat("a", slugMaxLen-1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AutoServerName(tc.rootAbs)
			wantPrefix := "dir2mcp-" + tc.wantSlug + "-"
			if !strings.HasPrefix(got, wantPrefix) {
				t.Fatalf("AutoServerName(%q) = %q, want prefix %q", tc.rootAbs, got, wantPrefix)
			}
			suffix := strings.TrimPrefix(got, wantPrefix)
			if len(suffix) != hashLen {
				t.Fatalf("AutoServerName(%q) hash suffix = %q (len %d), want len %d", tc.rootAbs, suffix, len(suffix), hashLen)
			}
			for _, r := range suffix {
				if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
					t.Fatalf("AutoServerName(%q) suffix %q contains non-hex rune %q", tc.rootAbs, suffix, r)
				}
			}
		})
	}
}

func TestAutoServerNameDeterministic(t *testing.T) {
	a := AutoServerName("/home/user/Stas Legal")
	b := AutoServerName("/home/user/Stas Legal")
	if a != b {
		t.Fatalf("AutoServerName not deterministic: %q vs %q", a, b)
	}
}

func TestAutoServerNameDistinctPathsDistinctNames(t *testing.T) {
	a := AutoServerName("/home/user/Notes")
	b := AutoServerName("/home/user/notes")
	if a == b {
		t.Fatalf("AutoServerName should differ for case-distinct paths, got %q for both", a)
	}
	c := AutoServerName("/home/userA/notes")
	d := AutoServerName("/home/userB/notes")
	if c == d {
		t.Fatalf("AutoServerName should differ for distinct parent dirs, got %q for both", c)
	}
}

func TestResolve(t *testing.T) {
	cases := []struct {
		name     string
		rootAbs  string
		override string
		want     string
	}{
		{"override wins", "/home/user/notes", "myalias", "myalias"},
		{"override trimmed", "/home/user/notes", "  alias  ", "alias"},
		{"empty override falls through", "/home/user/notes", "", AutoServerName("/home/user/notes")},
		{"whitespace-only override falls through", "/home/user/notes", "   ", AutoServerName("/home/user/notes")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Resolve(tc.rootAbs, tc.override); got != tc.want {
				t.Fatalf("Resolve(%q, %q) = %q, want %q", tc.rootAbs, tc.override, got, tc.want)
			}
		})
	}
}

func TestAutoServerNameTrailingSlashEqualsClean(t *testing.T) {
	a := AutoServerName("/var/lib/foo")
	b := AutoServerName("/var/lib/foo/")
	if a != b {
		t.Fatalf("AutoServerName should normalize trailing slash; got %q vs %q", a, b)
	}
}
