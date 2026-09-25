package contextfs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var obsDay = time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)

func obs(claim, source string) Observation {
	return Observation{Date: obsDay, Claim: claim, Source: source}
}

// TestObservedRoundTripPreservesHandWrittenContent is the test this file
// exists for.
//
// PROFILE.md and SKILLS.md are hand-authored and irreplaceable, and Apex
// rewrites them. Every other failure in this package is recoverable; losing a
// paragraph the user wrote is not. So the assertion is not "the block came out
// right" but "every byte the user wrote is still there, in order", checked
// across the shapes a real file actually takes.
func TestObservedRoundTripPreservesHandWrittenContent(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"no block at all", "# Me\n\nI work on personal tools.\n\n## How I work\n\nSlowly.\n"},
		{"empty block the user created by hand", "# Me\n\nProse.\n\n## Observed\n"},
		{"block at the end", "# Me\n\nProse.\n\n## Observed\n" +
			"<!--apex 2026-09-01--> Old claim — source: somewhere.\n"},
		{"block followed by another section", "# Me\n\nProse.\n\n## Observed\n" +
			"<!--apex 2026-09-01--> Old claim.\n\n## Notes\n\nMine, not Apex's.\n"},
		{"crlf throughout", "# Me\r\n\r\nProse.\r\n\r\n## Observed\r\n" +
			"<!--apex 2026-09-01--> Old claim.\r\n"},
		{"no trailing newline", "# Me\n\nProse I did not terminate."},
		{"empty file", ""},
		{"fenced block that looks like a heading", "# Me\n\n```\n## Observed\n```\n\nReal prose.\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ParseIdentityFile([]byte(tc.body), "PROFILE.md")
			before := userLines(t, tc.body)

			f.SetObservations([]Observation{
				obs("Prefers pixel art for UI work", "said while reviewing a mockup"),
				obs("Builds CLIs in Go or Rust by default", "asked for a stack recommendation"),
			})

			after := userLines(t, string(f.Bytes()))
			if len(before) != len(after) {
				t.Fatalf("hand-written lines changed count: %d -> %d\nbefore %q\nafter  %q",
					len(before), len(after), before, after)
			}
			for i := range before {
				if before[i] != after[i] {
					t.Fatalf("hand-written line %d changed:\n  before %q\n  after  %q", i, before[i], after[i])
				}
			}

			// And the observations really did land.
			got := ParseIdentityFile(f.Bytes(), "PROFILE.md").Observations()
			if len(got) != 2 {
				t.Fatalf("read back %d observation(s), want 2:\n%s", len(got), f.Bytes())
			}
			if got[0].Claim != "Prefers pixel art for UI work" {
				t.Errorf("first claim = %q", got[0].Claim)
			}
			if got[0].Source != "said while reviewing a mockup" {
				t.Errorf("first source = %q; the source is what makes a wrong inference debuggable", got[0].Source)
			}
		})
	}
}

// userLines returns every line that is not Apex's: not a marked entry, not the
// heading, not the note. Those are the bytes that must survive untouched.
func userLines(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, raw := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || trimmed == ObservedNote {
			continue
		}
		if isObservationLine(trimmed) {
			continue
		}
		if name, ok := parseHeading(trimmed); ok && strings.EqualFold(name, ObservedHeading) {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

func TestSetObservationsCreatesTheBlockOnlyWhenThereIsSomethingToSay(t *testing.T) {
	f := ParseIdentityFile([]byte("# Me\n\nProse.\n"), "PROFILE.md")
	f.SetObservations(nil)
	if f.HasBlock() {
		t.Fatal("an empty observation set created a ## Observed section in a file the user owns")
	}
	if f.Changed() {
		t.Fatalf("nothing to write, but the file changed:\n%s", f.Bytes())
	}

	f.SetObservations([]Observation{obs("Builds CLIs in Go", "a stack question")})
	if !f.HasBlock() {
		t.Fatal("writing an observation did not create the block")
	}
	if !strings.Contains(string(f.Bytes()), ObservedNote) {
		t.Error("a freshly created block carries no note explaining who owns it")
	}
}

// TestUnmarkedLinesInsideTheBlockSurvive holds the same line registry.go holds
// for summaries: an unmarked line is the user's prose, wherever it sits.
func TestUnmarkedLinesInsideTheBlockSurvive(t *testing.T) {
	body := "# Me\n\n## Observed\n" +
		"<!--apex 2026-09-01--> Old claim.\n" +
		"I added this note by hand and I want it kept.\n"
	f := ParseIdentityFile([]byte(body), "PROFILE.md")
	f.SetObservations([]Observation{obs("New claim", "a remark")})

	out := string(f.Bytes())
	if !strings.Contains(out, "I added this note by hand and I want it kept.") {
		t.Fatalf("an unmarked line inside the block was deleted:\n%s", out)
	}
	if strings.Contains(out, "Old claim") {
		t.Fatalf("a superseded marked line survived:\n%s", out)
	}
}

func TestSetObservationsIsIdempotent(t *testing.T) {
	body := "# Me\n\nProse.\n"
	want := []Observation{obs("A", "x"), obs("B", "y")}

	f := ParseIdentityFile([]byte(body), "PROFILE.md")
	f.SetObservations(want)
	once := string(f.Bytes())

	g := ParseIdentityFile([]byte(once), "PROFILE.md")
	g.SetObservations(want)
	twice := string(g.Bytes())

	if once != twice {
		t.Fatalf("repeated writes accrete lines:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
	if g.Changed() {
		t.Error("rewriting the same observations reports the file as changed")
	}
}

// TestMalformedMarkerIsLeftAlone: a line Apex cannot read as an entry is more
// likely the user's than its own, so it is never rewritten or removed.
func TestMalformedMarkerIsLeftAlone(t *testing.T) {
	body := "## Observed\n" +
		"<!--apex--> this is the registry's marker, not an observation\n" +
		"<!--apex not-a-date--> nor is this\n" +
		"<!--apex 2026-09-01--> but this is.\n"
	f := ParseIdentityFile([]byte(body), "PROFILE.md")
	if got := len(f.Observations()); got != 1 {
		t.Fatalf("parsed %d observation(s), want 1", got)
	}
	f.SetObservations([]Observation{obs("Replacement", "")})
	out := string(f.Bytes())
	for _, keep := range []string{"<!--apex--> this is the registry's marker", "<!--apex not-a-date-->"} {
		if !strings.Contains(out, keep) {
			t.Errorf("a line Apex could not parse was destroyed: %q\n%s", keep, out)
		}
	}
}

func TestObservationsAreCapped(t *testing.T) {
	many := make([]Observation, MaxObservations+5)
	for i := range many {
		many[i] = obs("claim "+string(rune('a'+i)), "source")
	}
	f := ParseIdentityFile([]byte("# Me\n"), "PROFILE.md")
	f.SetObservations(many)

	got := f.Observations()
	if len(got) != MaxObservations {
		t.Fatalf("wrote %d observations, want the cap of %d", len(got), MaxObservations)
	}
	// The cap keeps the head, so a caller ordering newest-first keeps the
	// newest rather than whatever happened to sort last.
	if got[0].Claim != many[0].Claim {
		t.Errorf("cap dropped from the wrong end: kept %q first", got[0].Claim)
	}
}

func TestObservedBlockEndsAtTheNextHeading(t *testing.T) {
	body := "## Observed\n" +
		"<!--apex 2026-09-01--> Inside.\n" +
		"## Later\n" +
		"<!--apex 2026-09-02--> Outside, and not Apex's to touch.\n"
	f := ParseIdentityFile([]byte(body), "SKILLS.md")
	got := f.Observations()
	if len(got) != 1 || got[0].Claim != "Inside." {
		t.Fatalf("observations = %+v, want only the one inside the block", got)
	}
	f.SetObservations([]Observation{obs("Replacement", "")})
	if !strings.Contains(string(f.Bytes()), "Outside, and not Apex's to touch.") {
		t.Errorf("a marked line outside the block was rewritten:\n%s", f.Bytes())
	}
}

func TestUserBodyExcludesApexsBlock(t *testing.T) {
	body := "# Me\n\nI care about tooling.\n\n## Observed\n" +
		ObservedNote + "\n<!--apex 2026-09-01--> Something Apex inferred.\n"
	f := ParseIdentityFile([]byte(body), "PROFILE.md")
	user := f.UserBody()
	if strings.Contains(user, "Something Apex inferred") || strings.Contains(user, ObservedHeading) {
		t.Fatalf("UserBody leaked Apex's block:\n%s", user)
	}
	if !strings.Contains(user, "I care about tooling.") {
		t.Fatalf("UserBody dropped the user's prose:\n%s", user)
	}
}

func TestSaveIsAtomicAndSkipsUnchangedFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ProfileFile)
	original := "# Me\n\nProse.\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	f, err := LoadIdentityFile(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != original {
		t.Fatalf("an unchanged save rewrote the file:\n%s", got)
	}

	f.SetObservations([]Observation{obs("Writes Go", "a stack question")})
	if err := f.Save(ctx); err != nil {
		t.Fatal(err)
	}
	// No temp file may survive beside the target: a leftover .PROFILE.md.tmp
	// in the user's context directory is litter Apex would never clean up.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != ProfileFile {
			t.Errorf("Save left %s behind", e.Name())
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), original) {
		t.Fatalf("the user's prose is no longer at the top of the file:\n%s", got)
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm() != 0o600 {
		t.Errorf("permissions changed to %v; identity context is single-user", info.Mode().Perm())
	}
}

func TestObservationTextSanitisesItsFields(t *testing.T) {
	o := Observation{
		Date:   obsDay,
		Claim:  "Likes\npixel art <!--apex 2020-01-01-->",
		Source: "said  while\treviewing",
	}
	line := o.Line()
	if strings.Count(line, "-->") != 1 {
		t.Fatalf("a smuggled marker survived into the line: %q", line)
	}
	if strings.ContainsAny(line, "\n\t") {
		t.Fatalf("an entry spans more than one line: %q", line)
	}
	back, ok := parseObservation(line)
	if !ok {
		t.Fatalf("the rendered line does not parse back: %q", line)
	}
	if back.Source != "said while reviewing" {
		t.Errorf("source round trip = %q", back.Source)
	}
}
