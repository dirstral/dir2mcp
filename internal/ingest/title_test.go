package ingest

import "testing"

func TestExtractTitle(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "markdown h1",
			body: "# Document Title\n\nbody text",
			want: "Document Title",
		},
		{
			name: "markdown h2",
			body: "## Subtitle Heading\nmore body",
			want: "Subtitle Heading",
		},
		{
			name: "OCR'd legal PDF: heading wins over earlier all-caps jurisdiction header",
			body: "VIRGIN ISLANDS\n\n# PROLIFERATION FINANCING (PROHIBITION) ACT, 2021\n\narrangement of sections...",
			want: "PROLIFERATION FINANCING (PROHIBITION) ACT, 2021",
		},
		{
			name: "uppercase fallback when no heading is present",
			body: "VIRGIN ISLANDS\n\nsome body text that follows\n",
			want: "VIRGIN ISLANDS",
		},
		{
			name: "leading whitespace and blank lines are skipped",
			body: "\n\n   \n# Title After Blanks\n",
			want: "Title After Blanks",
		},
		{
			name: "lowercase prose is rejected",
			body: "this is just a sentence in normal prose. It is not a title.",
			want: "",
		},
		{
			name: "uppercase line ending in a period is rejected (looks like a sentence)",
			body: "THIS IS A SHOUTED SENTENCE.\n",
			want: "",
		},
		{
			name: "mostly-uppercase line shorter than 3 letters is rejected",
			body: "X\n",
			want: "",
		},
		{
			name: "title is clamped to titleMaxLen",
			body: "# " + repeat("A", 500),
			want: repeat("A", titleMaxLen),
		},
		{
			name: "empty body returns empty",
			body: "",
			want: "",
		},
		{
			name: "#442: shebang is not a markdown heading",
			body: "#!/usr/bin/env python\nprint('hello world')\n",
			want: "",
		},
		{
			name: "#442: hashtag is not a markdown heading",
			body: "#nowplaying just vibing to some lo-fi beats\nmore lowercase body\n",
			want: "",
		},
		{
			name: "#442: YAML front-matter title wins outright",
			body: "---\ntitle: The Real Title\n---\nCHAPTER ONE\n\nbody text\n",
			want: "The Real Title",
		},
		{
			name: "#442: front-matter title strips surrounding quotes",
			body: "---\nauthor: Jane Doe\ntitle: \"Quoted Title\"\n---\nbody\n",
			want: "Quoted Title",
		},
		{
			name: "#442: front-matter without a title key falls through to heading",
			body: "---\nauthor: Jane Doe\ndate: 2026-06-25\n---\n# Fallback Heading\n",
			want: "Fallback Heading",
		},
		{
			name: "#442: bare leading --- (no close) is not front-matter; heading still wins",
			body: "---\n# Just A Heading\n",
			want: "Just A Heading",
		},
		{
			name: "#442: digit-heavy page header is not a title",
			body: "PAGE 1 OF 10\n\nsome lowercase body that follows the header\n",
			want: "",
		},
		{
			name: "#442: month/year running header is not a title",
			body: "JANUARY 2026\n\nsome lowercase body that follows the header\n",
			want: "",
		},
		{
			name: "#442: colon-terminated label is not a title",
			body: "ABSTRACT:\nthis is the abstract body, all lowercase prose.\n",
			want: "",
		},
		{
			name: "#442: genuine all-caps title still works",
			body: "MAIN TITLE\n\nsome lowercase body text\n",
			want: "MAIN TITLE",
		},
		{
			name: "#442: all-caps title with a trailing year still works",
			body: "PROLIFERATION FINANCING (PROHIBITION) ACT, 2021\n\nbody\n",
			want: "PROLIFERATION FINANCING (PROHIBITION) ACT, 2021",
		},
		{
			name: "#442: # comment inside a code fence is not a heading",
			body: "```python\n# this is a code comment, not a heading\nx = 1\n```\n\nMAIN TITLE\n",
			want: "MAIN TITLE",
		},
		{
			name: "#442: setext H1 underline is recognized",
			body: "The Setext Title\n================\n\nbody text\n",
			want: "The Setext Title",
		},
		{
			name: "scan limit honored — late title is found if before limit",
			body: padding(titleScanLimit-200) + "# Late Title Within Window",
			want: "Late Title Within Window",
		},
		{
			name: "scan limit honored — title past limit is NOT found",
			body: padding(titleScanLimit+200) + "# Title Past The Limit",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractTitle(tc.body)
			if got != tc.want {
				t.Errorf("ExtractTitle(...)\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

func padding(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = ' '
	}
	return string(out)
}
