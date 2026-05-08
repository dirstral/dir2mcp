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
			name: "uppercase act title typical of OCR'd legal PDF",
			body: "VIRGIN ISLANDS\n\n# PROLIFERATION FINANCING (PROHIBITION) ACT, 2021\n\narrangement of sections...",
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
