package main

import (
	"strings"
	"testing"
)

// TestRenderMarkdown covers the shapes an action item body actually arrives
// in. The bug this guards against is real: an indented paragraph whose
// continuation lines lost the indent, which the first live `apex review`
// printed before it was fixed.
func TestRenderMarkdown(t *testing.T) {
	for _, tc := range []struct {
		name, src, indent string
		width             int
		want              string
	}{
		{
			name:   "wrapped paragraph keeps its indent",
			src:    "one two three four five six seven eight",
			indent: "  ",
			width:  20,
			want:   "  one two three four\n  five six seven\n  eight\n",
		},
		{
			name:   "list items hang under their bullet",
			src:    "- alpha beta gamma delta\n- short",
			indent: "",
			width:  16,
			want:   "- alpha beta\n  gamma delta\n- short\n",
		},
		{
			name:   "emphasis is stripped, code spans are not",
			src:    "run **now** with `--flag`",
			indent: "",
			width:  80,
			want:   "run now with `--flag`\n",
		},
		{
			name:   "fenced code is verbatim, not wrapped",
			src:    "do this:\n\n```\na very long command that would otherwise be wrapped\n```",
			indent: "",
			width:  20,
			want:   "do this:\n\n    a very long command that would otherwise be wrapped\n",
		},
		{
			name:   "a heading is its own line",
			src:    "## Acceptance\n\nIt passes.",
			indent: "",
			width:  40,
			want:   "Acceptance\n\nIt passes.\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			renderMarkdown(&b, tc.src, tc.indent, tc.width)
			if got := b.String(); got != tc.want {
				t.Errorf("renderMarkdown =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// TestNormalizeID checks the id forms `apex show` accepts. The padding is not
// cosmetic: store.nextID writes AI-001, so an unpadded lookup finds nothing.
func TestNormalizeID(t *testing.T) {
	for _, tc := range []struct{ in, item, idea string }{
		{"AI-014", "AI-014", ""},
		{"ai-14", "AI-014", ""},
		{"ai14", "AI-014", ""},
		{"IDEA-7", "", "IDEA-007"},
		{"idea7", "", "IDEA-007"},
		{"7", "AI-007", "IDEA-007"}, // ambiguous: both are looked up
		{"nonsense", "", ""},
	} {
		item, idea := normalizeID(tc.in)
		if item != tc.item || idea != tc.idea {
			t.Errorf("normalizeID(%q) = (%q, %q), want (%q, %q)", tc.in, item, idea, tc.item, tc.idea)
		}
	}
}
