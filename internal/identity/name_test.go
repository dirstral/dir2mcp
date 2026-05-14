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
			got := AutoServerName(tc.rootAbs, false)
			wantPrefix := releasePrefix + "-" + tc.wantSlug + "-"
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

func TestAutoServerNameDevPrefix(t *testing.T) {
	const path = "/home/user/Stas Legal"
	release := AutoServerName(path, false)
	dev := AutoServerName(path, true)

	if !strings.HasPrefix(release, releasePrefix+"-") {
		t.Fatalf("release name %q lacks release prefix %q", release, releasePrefix)
	}
	if !strings.HasPrefix(dev, devPrefix+"-") {
		t.Fatalf("dev name %q lacks dev prefix %q", dev, devPrefix)
	}
	if release == dev {
		t.Fatalf("release and dev names should differ for the same path; got %q for both", release)
	}
	// Slug + hash suffix should be identical between release and dev —
	// the only difference is the prefix segment.
	releaseTail := strings.TrimPrefix(release, releasePrefix+"-")
	devTail := strings.TrimPrefix(dev, devPrefix+"-")
	if releaseTail != devTail {
		t.Fatalf("dev and release should share slug+hash tail; got release=%q dev=%q", releaseTail, devTail)
	}
}

func TestAutoServerNameDeterministic(t *testing.T) {
	for _, dev := range []bool{false, true} {
		a := AutoServerName("/home/user/Stas Legal", dev)
		b := AutoServerName("/home/user/Stas Legal", dev)
		if a != b {
			t.Fatalf("AutoServerName not deterministic for dev=%v: %q vs %q", dev, a, b)
		}
	}
}

func TestAutoServerNameDistinctPathsDistinctNames(t *testing.T) {
	a := AutoServerName("/home/user/Notes", false)
	b := AutoServerName("/home/user/notes", false)
	if a == b {
		t.Fatalf("AutoServerName should differ for case-distinct paths, got %q for both", a)
	}
	c := AutoServerName("/home/userA/notes", false)
	d := AutoServerName("/home/userB/notes", false)
	if c == d {
		t.Fatalf("AutoServerName should differ for distinct parent dirs, got %q for both", c)
	}
}

func TestResolve(t *testing.T) {
	cases := []struct {
		name     string
		rootAbs  string
		override string
		dev      bool
		want     string
	}{
		{"override wins (release)", "/home/user/notes", "myalias", false, "myalias"},
		{"override wins (dev)", "/home/user/notes", "myalias", true, "myalias"},
		{"override trimmed", "/home/user/notes", "  alias  ", false, "alias"},
		{"empty override falls through (release)", "/home/user/notes", "", false, AutoServerName("/home/user/notes", false)},
		{"empty override falls through (dev)", "/home/user/notes", "", true, AutoServerName("/home/user/notes", true)},
		{"whitespace-only override falls through", "/home/user/notes", "   ", false, AutoServerName("/home/user/notes", false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Resolve(tc.rootAbs, tc.override, tc.dev); got != tc.want {
				t.Fatalf("Resolve(%q, %q, dev=%v) = %q, want %q", tc.rootAbs, tc.override, tc.dev, got, tc.want)
			}
		})
	}
}

func TestAutoServerNameTrailingSlashEqualsClean(t *testing.T) {
	a := AutoServerName("/var/lib/foo", false)
	b := AutoServerName("/var/lib/foo/", false)
	if a != b {
		t.Fatalf("AutoServerName should normalize trailing slash; got %q vs %q", a, b)
	}
}
