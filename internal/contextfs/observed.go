package contextfs

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Learned observations (DESIGN.md §6).
//
// Apex owns one section of PROFILE.md and SKILLS.md — a trailing `## Observed`
// block — and nothing else. Every entry in it carries a marker and a date;
// everything outside it belongs to the user and is never touched.
//
// The hazard is identical to PROJECTS.md's: a file the user hand-edits and
// Apex also writes. So the mechanics are identical too, and deliberately reuse
// this package's existing machinery rather than growing a second copy —
// splitLines/joinLines for the byte-exact round trip, writeAtomic for the
// write, parseHeading for structure. The failure this prevents is the only one
// that really matters here: losing prose the user wrote by hand and cannot get
// back.
//
//	## Observed
//	<!-- Apex maintains the entries below. Everything else in this file is yours. -->
//	<!--apex 2026-09-25--> Prefers pixel art for UI work — source: said while reviewing a mockup.
//	<!--apex 2026-09-21--> Builds CLIs in Go or Rust by default — source: asked for a stack recommendation.
//
// Two departures from the sketch in §6 are worth naming.
//
// The source is separated from the claim by an explicit " — source: " rather
// than by the semicolon §6's example uses. §6 requires the source to be
// recorded and read back — `apex profile` lists observations "with dates and
// sources" — and a semicolon cannot be parsed back out of a claim that
// contains one. The separator is visible prose rather than a hidden comment
// attribute on purpose: this text is re-sent to the model on every advisor
// call, and "said while reviewing one mockup" is exactly the qualifier that
// stops a one-off remark being read as a standing preference.
//
// And the block is found by its heading, not by being last in the file. A user
// who adds a section below it keeps it; Apex's block simply ends where the
// next heading begins.

// ObservedHeading is the level-2 heading naming Apex's block.
const ObservedHeading = "Observed"

// ObservedNote is the explanatory comment Apex writes under a block it had to
// create. It is never required: a block without it parses the same way.
const ObservedNote = "<!-- Apex maintains the entries below. Everything else in this file is yours. -->"

// ObservationDateLayout is the date format inside a marker.
const ObservationDateLayout = "2006-01-02"

// ObservationSourceSeparator divides a claim from the source it came from.
const ObservationSourceSeparator = " — source: "

const (
	observationMarkerOpen  = "<!--apex "
	observationMarkerClose = "-->"
)

// MaxObservations caps one file's block.
//
// This is not tidiness. Identity context is loaded into the system prompt of
// every advisor call, and DESIGN.md §8 established by measurement that prompt
// caching does not work on the subscription transport — every call writes the
// cache and reads none. An unbounded `## Observed` block is therefore a
// compounding cost paid on every call, forever, and the only defence is a cap.
//
// Twelve entries per file is roughly 250 tokens across both, which is a
// reasonable standing price for context that shapes every answer.
const MaxObservations = 12

// Observation is one learned fact about the user.
type Observation struct {
	// Date is the day it was recorded.
	Date time.Time
	// Claim is what Apex believes.
	Claim string
	// Source is where the belief came from — the remark or the situation.
	//
	// DESIGN.md §6 calls this the thing that makes a wrong inference
	// debuggable rather than mysterious, and it is why the claim is never
	// stored alone: "likes pixel art" said about one mockup is not a general
	// aesthetic, and nothing else in the file would reveal that.
	Source string
}

// Text renders the visible half of an entry: the claim, and the source when
// there is one.
func (o Observation) Text() string {
	claim := sanitizeObservationField(o.Claim)
	source := sanitizeObservationField(o.Source)
	if source == "" {
		return claim
	}
	return claim + ObservationSourceSeparator + source
}

// Line renders a whole entry, marker included, without a terminator.
func (o Observation) Line() string {
	date := o.Date
	if date.IsZero() {
		date = time.Now()
	}
	return observationMarkerOpen + date.Format(ObservationDateLayout) +
		observationMarkerClose + " " + o.Text()
}

// Valid reports whether an observation carries a claim worth writing.
func (o Observation) Valid() bool { return sanitizeObservationField(o.Claim) != "" }

// sanitizeObservationField folds a field onto one line and strips anything
// that would change how the file parses on the next read: newlines, and the
// marker itself.
func sanitizeObservationField(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, observationMarkerOpen, "")
	s = strings.ReplaceAll(s, observationMarkerClose, "")
	s = strings.ReplaceAll(s, ObservationSourceSeparator, " ")
	return strings.Join(strings.Fields(s), " ")
}

// AuthoredBody returns a document with Apex's `## Observed` block removed:
// what the user wrote themselves.
//
// It exists because of a question two parts of the spec answer differently.
// §6 wants the whole file — observations included — loaded into every advisor
// prompt, so Document.Empty is about the prompt and must stay as it is. But
// §18 item 13 wants `doctor` to warn when there is no identity context, on the
// grounds that "an empty identity context still produces output, it is just
// generic advice about code rather than advice for this user, and that failure
// is invisible from the outside". Once Apex writes into these files, Empty
// stops being able to answer that second question: a PROFILE.md containing
// nothing but two observations Apex inferred is not absent, and it is also not
// identity context the user provided. Asking the wrong one would silence the
// warning exactly when it started mattering.
func (d Document) AuthoredBody() string {
	if !d.Present {
		return ""
	}
	return ParseIdentityFile([]byte(d.Body), d.Path).UserBody()
}

// AuthoredEmpty reports whether the user has written nothing of their own in
// this document, whatever Apex may have added to it.
func (d Document) AuthoredEmpty() bool { return strings.TrimSpace(d.AuthoredBody()) == "" }

// IdentityFile is a parsed PROFILE.md or SKILLS.md that remembers its exact
// bytes.
//
// Bytes returns the input unchanged until SetObservations is called, and
// SetObservations rewrites only marked lines inside the `## Observed` block.
// Everything else survives byte for byte, including CRLF terminators and a
// missing final newline.
type IdentityFile struct {
	// Path is the file this was loaded from.
	Path string
	// Present is false when the file does not exist. An absent identity file
	// is an empty one, not an error — the same rule LoadDocument follows.
	Present bool

	original []byte
	lines    []string

	obs      []Observation
	obsLines []int // indices into lines, in file order

	// heading is the index of the `## Observed` line, or -1 when the file
	// has no block yet. blockEnd is exclusive.
	heading  int
	blockEnd int
}

// LoadIdentityFile reads and parses PROFILE.md or SKILLS.md.
func LoadIdentityFile(ctx context.Context, path string) (*IdentityFile, error) {
	doc, err := LoadDocument(ctx, path)
	if err != nil {
		return nil, err
	}
	if !doc.Present {
		return &IdentityFile{Path: path, heading: -1, blockEnd: -1}, nil
	}
	f := ParseIdentityFile([]byte(doc.Body), path)
	f.Present = true
	return f, nil
}

// ParseIdentityFile parses identity-file bytes that are already in hand.
//
// Parsing never fails. A marked line Apex cannot read as an entry is left
// alone rather than rewritten, because a line it does not understand is more
// likely to be the user's than its own.
func ParseIdentityFile(data []byte, path string) *IdentityFile {
	f := &IdentityFile{
		Path:     path,
		Present:  true,
		original: append([]byte(nil), data...),
		lines:    splitLines(data),
	}
	f.parse()
	return f
}

// Observations returns the entries in Apex's block, in file order.
func (f *IdentityFile) Observations() []Observation {
	out := make([]Observation, len(f.obs))
	copy(out, f.obs)
	return out
}

// HasBlock reports whether the file already carries a `## Observed` section.
func (f *IdentityFile) HasBlock() bool { return f.heading >= 0 }

// UserBody returns everything the user wrote: the file with Apex's block
// removed.
//
// DESIGN.md §6 wants an observation the user has promoted into their own prose
// dropped from the block, and an extractor cannot notice that without being
// shown the prose separately from the block it is about to rewrite.
func (f *IdentityFile) UserBody() string {
	if f.heading < 0 {
		return string(joinLines(f.lines))
	}
	kept := make([]string, 0, len(f.lines))
	kept = append(kept, f.lines[:f.heading]...)
	kept = append(kept, f.lines[f.blockEnd:]...)
	return strings.TrimSpace(string(joinLines(kept)))
}

// Bytes renders the file.
func (f *IdentityFile) Bytes() []byte { return joinLines(f.lines) }

// Changed reports whether the file differs from the bytes it was parsed from.
func (f *IdentityFile) Changed() bool { return string(f.original) != string(f.Bytes()) }

// Save writes the file back atomically, and does nothing when nothing changed.
//
// Atomic because DESIGN.md §6 requires it and because the consequence is
// unrecoverable: an interrupted write of a hand-authored identity file
// truncates work the user cannot regenerate.
func (f *IdentityFile) Save(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.Path == "" {
		return fmt.Errorf("contextfs: no path recorded for this identity file")
	}
	if f.Present && !f.Changed() {
		return nil
	}
	data := f.Bytes()
	if err := writeAtomic(f.Path, data); err != nil {
		return fmt.Errorf("write %s: %w", f.Path, err)
	}
	f.original = append([]byte(nil), data...)
	f.Present = true
	return nil
}

// SetObservations replaces the entries in Apex's block.
//
// Only marked lines inside the `## Observed` block are removed. Unmarked lines
// inside it — a note the user added under the heading — stay exactly where
// they are, on the same principle SetSummary follows in registry.go: an
// unmarked line is the user's prose and is never adopted, never rewritten and
// never deleted.
//
// Entries past MaxObservations are dropped from the end, so a caller that
// orders newest first keeps the newest.
func (f *IdentityFile) SetObservations(obs []Observation) {
	kept := make([]Observation, 0, len(obs))
	for _, o := range obs {
		if o.Valid() {
			kept = append(kept, o)
		}
	}
	if len(kept) > MaxObservations {
		kept = kept[:MaxObservations]
	}
	if len(kept) == 0 && f.heading < 0 {
		// Nothing to write and nowhere to write it. Creating an empty
		// section in a file the user owns would be a change for its own
		// sake.
		return
	}

	ending := f.defaultEnding()
	rendered := make([]string, 0, len(kept))
	for _, o := range kept {
		rendered = append(rendered, o.Line()+ending)
	}

	if f.heading < 0 {
		f.appendBlock(rendered, ending)
		f.parse()
		return
	}

	// Where the entries go: directly under the heading, after an
	// explanatory comment if one is sitting there.
	insert := f.heading + 1
	if insert < f.blockEnd {
		text := strings.TrimSpace(lineText(f.lines[insert]))
		if strings.HasPrefix(text, "<!--") && !isObservationLine(text) {
			insert++
		}
	}

	// A heading that ended the file without a newline has to be terminated
	// before anything can follow it.
	if lineEnding(f.lines[f.heading]) == "" {
		f.lines[f.heading] += ending
	}

	drop := make(map[int]bool, len(f.obsLines))
	for _, i := range f.obsLines {
		drop[i] = true
	}
	next := make([]string, 0, len(f.lines)+len(rendered))
	for i, line := range f.lines {
		if i == insert {
			next = append(next, rendered...)
		}
		if drop[i] {
			continue
		}
		next = append(next, line)
	}
	if insert >= len(f.lines) {
		next = append(next, rendered...)
	}
	f.lines = next
	f.parse()
}

// appendBlock writes a new `## Observed` section at the end of the file.
func (f *IdentityFile) appendBlock(rendered []string, ending string) {
	if n := len(f.lines); n > 0 {
		if lineEnding(f.lines[n-1]) == "" {
			f.lines[n-1] += ending
		}
		if strings.TrimSpace(lineText(f.lines[n-1])) != "" {
			f.lines = append(f.lines, ending)
		}
	}
	f.lines = append(f.lines, "## "+ObservedHeading+ending, ObservedNote+ending)
	f.lines = append(f.lines, rendered...)
}

// defaultEnding is the terminator used for inserted lines: whatever the file
// already uses, so a CRLF file stays CRLF.
func (f *IdentityFile) defaultEnding() string {
	for _, l := range f.lines {
		if e := lineEnding(l); e != "" {
			return e
		}
	}
	return "\n"
}

// parse rebuilds the block's bounds and its entries from lines. It runs again
// after every mutation: inserting a line shifts every index after it, and a
// stale index is how a rewriter corrupts a file.
func (f *IdentityFile) parse() {
	f.obs = nil
	f.obsLines = nil
	f.heading = -1
	f.blockEnd = -1

	inFence := false
	for i, raw := range f.lines {
		text := lineText(raw)
		if isFence(text) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if f.heading < 0 {
			if name, ok := parseHeading(text); ok && strings.EqualFold(name, ObservedHeading) {
				f.heading = i
			}
			continue
		}
		// Inside the block. It ends at the next heading of level 1 or 2;
		// a deeper heading is structure within the section.
		if _, ok := parseHeading(text); ok || isTopHeading(text) {
			f.blockEnd = i
			break
		}
		if obs, ok := parseObservation(text); ok {
			f.obs = append(f.obs, obs)
			f.obsLines = append(f.obsLines, i)
		}
	}
	if f.heading >= 0 && f.blockEnd < 0 {
		f.blockEnd = len(f.lines)
	}
}

// isTopHeading reports a level-1 heading, which also closes the block.
func isTopHeading(text string) bool {
	if !strings.HasPrefix(text, "#") || strings.HasPrefix(text, "##") {
		return false
	}
	rest := text[1:]
	return rest == "" || rest[0] == ' ' || rest[0] == '\t'
}

// isObservationLine reports whether a line is one of Apex's entries.
func isObservationLine(text string) bool {
	_, ok := parseObservation(text)
	return ok
}

// parseObservation reads one entry.
//
// A line that starts like a marker but does not carry a well-formed date is
// deliberately NOT an observation: it is left in place and never rewritten.
// Guessing at a line Apex does not recognise is how the round trip stops being
// safe.
func parseObservation(text string) (Observation, bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, observationMarkerOpen) {
		return Observation{}, false
	}
	rest := trimmed[len(observationMarkerOpen):]
	end := strings.Index(rest, observationMarkerClose)
	if end < 0 {
		return Observation{}, false
	}
	date, err := time.Parse(ObservationDateLayout, strings.TrimSpace(rest[:end]))
	if err != nil {
		return Observation{}, false
	}
	body := strings.TrimSpace(rest[end+len(observationMarkerClose):])
	obs := Observation{Date: date}
	if claim, source, found := strings.Cut(body, ObservationSourceSeparator); found {
		obs.Claim = strings.TrimSpace(claim)
		obs.Source = strings.TrimSpace(source)
	} else {
		obs.Claim = body
	}
	return obs, true
}
