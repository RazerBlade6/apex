package executor

import (
	"regexp"
	"strings"
	"testing"
)

// TestBriefCarriesIntentNotBackground is the test for DESIGN.md §9's central
// claim about briefs: the agent reads the project from the working tree, so
// the brief carries only what Apex knows and the tree does not.
func TestBriefCarriesIntentNotBackground(t *testing.T) {
	intent := Intent{
		Kind:        "action item",
		ID:          "AI-014",
		ProjectName: "ScholarRAG",
		Title:       "Make chunking table-aware",
		What:        "Split on table boundaries before splitting on length.",
		Why:         "The digest says numeric questions fail, and this is the named blocker.",
		Effort:      "medium",
	}
	brief := intent.Render()

	for _, want := range []string{
		"AI-014", "ScholarRAG",
		"Make chunking table-aware",
		"Split on table boundaries",
		"numeric questions fail",
		"medium",
		"Done means",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("the brief does not carry %q:\n%s", want, brief)
		}
	}

	// The standing rules that exist because of what DESIGN.md §14 says Apex
	// will not do. Losing either is not a cosmetic regression: an agent that
	// commits removes the diff the user reviews, and one that marks work done
	// takes a judgment that is explicitly the user's.
	if !strings.Contains(brief, "Do not commit") {
		t.Error("the brief does not tell the agent to leave the work uncommitted")
	}
	if !strings.Contains(strings.ToLower(brief), "do not mark the task complete") {
		t.Error("the brief does not tell the agent that completion is not its call")
	}

	// And the pointer-not-payload rule: the brief names the files to read and
	// does not inline them.
	withPaths := intent
	withPaths.ContextPaths = []string{"PROJECT.md"}
	if !strings.Contains(withPaths.Render(), "PROJECT.md") {
		t.Error("the brief does not point at the files worth reading first")
	}
}

// TestBriefFallsBackToDefaultAcceptance: a brief with no acceptance criteria
// still says what done means, because "the agent decides" is the answer that
// makes unattended dispatch untrustworthy.
func TestBriefFallsBackToDefaultAcceptance(t *testing.T) {
	brief := Intent{Title: "Do the thing"}.Render()
	if !strings.Contains(brief, DefaultAcceptance[0]) {
		t.Errorf("a brief with no acceptance criteria did not fall back:\n%s", brief)
	}
}

// TestNewSessionIDIsAUUID guards the one property the CLI actually validates.
// A malformed --session-id is rejected before any work starts, and a
// duplicated one would attach this dispatch to another run's transcript.
func TestNewSessionIDIsAUUID(t *testing.T) {
	shape := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

	seen := map[string]bool{}
	for range 100 {
		id, err := NewSessionID()
		if err != nil {
			t.Fatalf("NewSessionID: %v", err)
		}
		if !shape.MatchString(id) {
			t.Fatalf("NewSessionID() = %q, want a version 4 UUID", id)
		}
		if seen[id] {
			t.Fatalf("NewSessionID() repeated %q", id)
		}
		seen[id] = true
	}
}
