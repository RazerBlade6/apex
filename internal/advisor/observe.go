package advisor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/RazerBlade6/apex/internal/contextfs"
	"github.com/RazerBlade6/apex/internal/provider"
)

// Learned observations (DESIGN.md §6).
//
// When a conversation reveals something durable about the user, Apex records
// it rather than losing it when the session ends. contextfs owns the file
// format and the round trip; this file owns the three judgments the spec
// actually argues about.
//
// Extraction is gated, not per-turn. A call after every chat turn doubles the
// cost of conversation, so a cheap screen runs first — first person plus a
// preference or capability verb — and a turn revealing nothing costs nothing.
//
// Observations are superseded, not stacked. "I'm learning Zig" followed months
// later by "I know Zig well now" must replace the earlier line, and §6 is
// explicit that contradiction handling belongs in extraction rather than in a
// later cleanup pass. The extractor is therefore shown every existing
// observation, numbered, and returns the numbers a new one replaces.
//
// The block is capped and consolidated. The cap lives in contextfs; the
// consolidation is the same mechanism as supersession — one new observation
// replacing two old ones — which is why the prompt asks for it in the same
// breath.

// ObservationRoute is the model slot extraction runs on.
//
// The digest slot, not the chat slot. Extraction is mechanical: read one turn,
// decide whether it states something durable, write a line. DESIGN.md §8 names
// the digest slot as the bulk, cost-lever route for exactly that kind of work,
// and routing extraction at the chat slot would spend the conversational model
// on a classification task. The gate is what bounds frequency; the route is
// what bounds unit cost, and both are needed.
const ObservationRoute = "digest"

// observeMaxTokens bounds an extraction. The output is a handful of one-line
// claims; anything longer is the model writing an essay about the user.
const observeMaxTokens = 1500

// ObservationFile names which identity document an observation belongs in.
const (
	ObservationProfile = "profile"
	ObservationSkills  = "skills"
)

// ObservationEntry is one observation with the number `apex profile` prints
// beside it.
//
// The numbering is flat across both files — profile first, then skills —
// because that is what `apex profile --forget <n>` indexes, and asking a user
// to disambiguate which file entry 3 is in would be a worse interface than
// having one list.
type ObservationEntry struct {
	N    int
	File string
	Obs  contextfs.Observation
}

// Observations is the pair of identity documents Apex may write to.
type Observations struct {
	Profile *contextfs.IdentityFile
	Skills  *contextfs.IdentityFile
}

// LoadObservations reads PROFILE.md and SKILLS.md.
//
// Neither being present is a valid state. Apex creates a `## Observed` block
// in a file that has none only when it has something to put in it.
func LoadObservations(ctx context.Context, root string) (*Observations, error) {
	profile, err := contextfs.LoadIdentityFile(ctx, contextfs.ProfilePath(root))
	if err != nil {
		return nil, err
	}
	skills, err := contextfs.LoadIdentityFile(ctx, contextfs.SkillsPath(root))
	if err != nil {
		return nil, err
	}
	return &Observations{Profile: profile, Skills: skills}, nil
}

// file returns the identity document for a name, or nil.
func (o *Observations) file(name string) *contextfs.IdentityFile {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case ObservationProfile:
		return o.Profile
	case ObservationSkills:
		return o.Skills
	default:
		return nil
	}
}

// List returns every observation, numbered from 1.
func (o *Observations) List() []ObservationEntry {
	var out []ObservationEntry
	n := 0
	for _, f := range []struct {
		name string
		file *contextfs.IdentityFile
	}{{ObservationProfile, o.Profile}, {ObservationSkills, o.Skills}} {
		for _, obs := range f.file.Observations() {
			n++
			out = append(out, ObservationEntry{N: n, File: f.name, Obs: obs})
		}
	}
	return out
}

// Save writes back whichever documents changed.
func (o *Observations) Save(ctx context.Context) error {
	if err := o.Profile.Save(ctx); err != nil {
		return err
	}
	return o.Skills.Save(ctx)
}

// Forget removes one observation by the number List gave it.
func (o *Observations) Forget(n int) (ObservationEntry, error) {
	list := o.List()
	if n < 1 || n > len(list) {
		return ObservationEntry{}, &UnknownObservationError{N: n, Count: len(list)}
	}
	target := list[n-1]
	o.replace(map[int]bool{n: true}, nil, list)
	return target, nil
}

// replace rewrites both files: every observation whose number is in `drop` is
// removed, and `added` is prepended to its file so the newest sits first.
func (o *Observations) replace(drop map[int]bool, added []ObservationEntry, list []ObservationEntry) {
	for _, name := range []string{ObservationProfile, ObservationSkills} {
		f := o.file(name)
		var kept []contextfs.Observation
		for _, e := range added {
			if e.File == name {
				kept = append(kept, e.Obs)
			}
		}
		for _, e := range list {
			if e.File != name || drop[e.N] {
				continue
			}
			kept = append(kept, e.Obs)
		}
		f.SetObservations(kept)
	}
}

// UnknownObservationError reports a --forget number that names nothing.
type UnknownObservationError struct{ N, Count int }

func (e *UnknownObservationError) Error() string {
	if e.Count == 0 {
		return fmt.Sprintf("there is no observation %d: apex has not recorded any yet", e.N)
	}
	return fmt.Sprintf("there is no observation %d: apex has recorded %d\n"+
		"  apex profile   lists them with their numbers", e.N, e.Count)
}

// UserFixable marks a bad number as something the user types, not a bug.
func (e *UnknownObservationError) UserFixable() bool { return true }

// --- the gate ---------------------------------------------------------------

// firstPerson is the set of tokens that make a sentence about the speaker.
var firstPerson = map[string]bool{
	"i": true, "im": true, "ive": true, "id": true, "ill": true,
	"my": true, "mine": true, "me": true, "myself": true,
}

// selfDescribing is the preference and capability vocabulary from DESIGN.md
// §6 — "I like / prefer / always / never / I use / I'm learning" — plus the
// obvious near-neighbours.
//
// It is deliberately a word list rather than a model call. The whole point of
// the gate is that a turn revealing nothing must cost nothing, and a gate that
// spent a call to decide whether to spend a call would be no gate at all.
var selfDescribing = map[string]bool{
	"like": true, "likes": true, "liked": true, "love": true, "loved": true,
	"hate": true, "hated": true, "dislike": true, "disliked": true,
	"prefer": true, "prefers": true, "preferred": true, "preference": true,
	"favourite": true, "favorite": true, "enjoy": true, "enjoyed": true,
	"always": true, "never": true, "usually": true, "generally": true,
	"tend": true, "rather": true, "default": true, "normally": true,
	"use": true, "uses": true, "used": true, "using": true,
	"write": true, "writing": true, "build": true, "building": true,
	"learning": true, "learnt": true, "learned": true, "know": true,
	"knows": true, "familiar": true, "comfortable": true, "rusty": true,
	"experienced": true, "avoid": true, "avoiding": true, "want": true,
	"switched": true, "switching": true, "care": true, "struggle": true,
	"good": true, "bad": true, "work": true, "working": true,
}

// ScreenTurn is the cheap gate: does this turn look like it says something
// durable about the user?
//
// Both halves are required. "always" alone is a word about a schedule; "I"
// alone is most of English. Together they are the shape of a standing
// preference or a capability claim, which is what §6 asks the screen to catch.
//
// False positives are acceptable — they cost one cheap call that returns
// nothing. A false negative loses a fact silently, so where the two trade off,
// this errs wide.
func ScreenTurn(text string) bool {
	var self, verb bool
	for _, word := range tokenize(text) {
		if firstPerson[word] {
			self = true
		}
		if selfDescribing[word] {
			verb = true
		}
		if self && verb {
			return true
		}
	}
	return false
}

// tokenize lowercases and splits on anything that is not a letter or digit,
// dropping apostrophes so "I'm" becomes "im" and "I've" becomes "ive".
func tokenize(s string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
		case r == '\'' || r == '’':
			// Kept inside the word, then dropped: "I'm" -> "im".
		default:
			flush()
		}
	}
	flush()
	return out
}

// --- extraction -------------------------------------------------------------

// Turn is one exchange the extractor reads.
type Turn struct {
	User      string
	Assistant string
}

// ExtractionResult is what one extraction did.
type ExtractionResult struct {
	// Screened is false when the cheap gate rejected the turn, in which case
	// no model call was made and everything else is zero.
	Screened bool
	// Called is false when the gate passed but there was nothing to send to
	// — an advisor with no provider, for instance.
	Called bool
	// Added are the observations written.
	Added []ObservationEntry
	// Superseded are the observations a new one replaced or that the
	// extractor dropped outright.
	Superseded []ObservationEntry
	Usage      provider.Usage
	Model      string
}

// Changed reports whether anything was written.
func (r *ExtractionResult) Changed() bool {
	return r != nil && (len(r.Added) > 0 || len(r.Superseded) > 0)
}

type observationResponse struct {
	Observations []extractedObservation `json:"observations"`
	Drop         []int                  `json:"drop"`
}

type extractedObservation struct {
	File     string `json:"file"`
	Claim    string `json:"claim"`
	Source   string `json:"source"`
	Replaces []int  `json:"replaces"`
}

// Observe extracts learned observations from one chat turn and writes them.
//
// It is the whole of §6's flow: screen, call, supersede, cap, save. The caller
// gets back what changed so it can say so — an observation recorded with no
// visible cause is exactly the "quietly skews every future suggestion" failure
// §6 warns about.
//
// It takes no *Context, and that absence is deliberate rather than an
// oversight. The prompt carries the existing observations and the user's own
// prose and nothing else: identity context and every project digest would add
// thousands of tokens to a call whose entire job is to classify one sentence,
// on a transport where §8 measured the prefix being re-sent at full price
// every time. Extraction reads a remark, not a portfolio.
func (a *Advisor) Observe(ctx context.Context, obs *Observations, turn Turn) (*ExtractionResult, error) {
	result := &ExtractionResult{}
	if !ScreenTurn(turn.User) {
		return result, nil
	}
	result.Screened = true

	schema, err := schemaBytes(observationsSchema)
	if err != nil {
		return result, err
	}
	route := a.Config.Models.Digest
	p, err := a.providerFor(ctx, route)
	if err != nil {
		return result, err
	}
	result.Called = true

	list := obs.List()
	prompt := provider.Prompt{
		Identity:    []provider.Section{{Title: "Your task", Body: observeInstructions}},
		Digests:     []provider.Section{{Title: "What Apex already believes", Body: renderObservationState(obs, list)}},
		Instruction: renderTurn(turn),
	}
	req := prompt.Build(route.Model, route.Effort, observeMaxTokens)

	var decoded observationResponse
	usage, err := p.Structured(ctx, req, schema, &decoded)
	result.Usage = usage
	result.Model = usage.Model
	if result.Model == "" {
		result.Model = route.Model
	}
	if err != nil {
		return result, err
	}

	now := a.now()
	drop := map[int]bool{}
	for _, n := range decoded.Drop {
		if n >= 1 && n <= len(list) {
			drop[n] = true
		}
	}

	var added []ObservationEntry
	for _, ex := range decoded.Observations {
		claim := strings.TrimSpace(ex.Claim)
		if claim == "" {
			continue
		}
		file := strings.ToLower(strings.TrimSpace(ex.File))
		if obs.file(file) == nil {
			// An unknown file is not a reason to lose the observation:
			// a preference with nowhere to go still belongs in PROFILE.md.
			file = ObservationProfile
		}
		for _, n := range ex.Replaces {
			if n >= 1 && n <= len(list) {
				drop[n] = true
			}
		}
		added = append(added, ObservationEntry{
			File: file,
			Obs: contextfs.Observation{
				Date:   now,
				Claim:  claim,
				Source: strings.TrimSpace(ex.Source),
			},
		})
	}

	if len(added) == 0 && len(drop) == 0 {
		return result, nil
	}

	nums := make([]int, 0, len(drop))
	for n := range drop {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	for _, n := range nums {
		result.Superseded = append(result.Superseded, list[n-1])
	}

	obs.replace(drop, added, list)
	if err := obs.Save(ctx); err != nil {
		return result, err
	}

	// Renumber what was written, so a caller printing the result shows the
	// numbers `apex profile --forget` will accept.
	byText := map[string]bool{}
	for _, e := range added {
		byText[e.File+"\x00"+e.Obs.Text()] = true
	}
	for _, e := range obs.List() {
		if byText[e.File+"\x00"+e.Obs.Text()] {
			result.Added = append(result.Added, e)
		}
	}
	return result, nil
}

// renderTurn is the volatile half of an extraction call.
func renderTurn(turn Turn) string {
	var b strings.Builder
	b.WriteString("The user said:\n\n")
	b.WriteString(strings.TrimSpace(turn.User))
	if reply := strings.TrimSpace(turn.Assistant); reply != "" {
		b.WriteString("\n\nApex replied (context only — never extract an observation from Apex's own words):\n\n")
		b.WriteString(clip(reply, 4000))
	}
	return b.String()
}

// renderObservationState shows the extractor what is already recorded and what
// the user wrote themselves.
//
// The user's own prose is included for the reason §6 gives: an observation the
// user has promoted into their own words should be dropped from the block, and
// an extractor that cannot see the prose cannot notice the duplication.
func renderObservationState(obs *Observations, list []ObservationEntry) string {
	var b strings.Builder
	b.WriteString("Existing observations, numbered. Use these numbers in \"replaces\" and \"drop\".\n\n")
	if len(list) == 0 {
		b.WriteString("(none yet)\n")
	}
	for _, e := range list {
		fmt.Fprintf(&b, "%d. [%s] %s\n", e.N, e.File, e.Obs.Text())
	}
	for _, f := range []struct {
		label string
		file  *contextfs.IdentityFile
	}{{"PROFILE.md", obs.Profile}, {"SKILLS.md", obs.Skills}} {
		body := strings.TrimSpace(f.file.UserBody())
		fmt.Fprintf(&b, "\n--- what the user wrote themselves in %s ---\n", f.label)
		if body == "" {
			b.WriteString("(empty)\n")
			continue
		}
		b.WriteString(clip(body, 6000) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// observeInstructions is the extraction system prompt.
const observeInstructions = `You maintain Apex's memory of one user. Apex is a personal
project-management tool that loads what it knows about the user into every
call it makes, so what you record here shapes every future answer.

You are given one turn of conversation and everything Apex already believes.
Return only what is DURABLE and NEW.

Record an observation when the user states something about themselves that
will still be true next month: a standing preference, a tool or language they
use by default, something they are learning, something they refuse to work
with, how they like to work.

Do NOT record:
- anything about the current task, question, or file;
- anything Apex said, as opposed to the user;
- a fact about a project rather than about the person;
- anything already covered by an existing observation or already written in
  the user's own prose above — say nothing rather than restating it.

Most turns yield nothing. An empty "observations" list is the correct and
common answer; padding it is worse than returning nothing, because a one-off
remark recorded as a standing preference skews every future suggestion with no
visible cause.

For each observation you do return:
- "claim" is one short sentence in the third person: "Builds CLIs in Go or
  Rust by default." Never "I build CLIs in Go."
- "source" says where the belief came from, in a few words: "said while
  choosing a stack for a new tool". This is what makes a wrong inference
  debuggable later, so it must describe the actual remark, not restate the
  claim.
- "file" is "skills" for what the user knows, is learning, or has experience
  with; "profile" for preferences, working style, and what they care about.
- "replaces" lists the numbers of existing observations this one supersedes.
  Use it when the new statement contradicts or updates an old one ("learning
  Zig" then "knows Zig well"), and use it to consolidate: one claim may
  replace two narrower ones.

"drop" lists observations that should simply be removed: no longer true, or
now stated in the user's own prose above.`

// observationsSchema constrains extraction output.
//
// Written to the intersection every vendor accepts, like the schemas beside it
// in schema.go: object root, every property required, additionalProperties
// false throughout.
const observationsSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["observations", "drop"],
  "properties": {
    "observations": {
      "type": "array",
      "description": "Durable new facts about the user. Usually empty.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["file", "claim", "source", "replaces"],
        "properties": {
          "file": {
            "type": "string",
            "enum": ["profile", "skills"],
            "description": "skills for knowledge and experience; profile for preferences and working style."
          },
          "claim": {
            "type": "string",
            "description": "One short third-person sentence."
          },
          "source": {
            "type": "string",
            "description": "Where this belief came from, in a few words."
          },
          "replaces": {
            "type": "array",
            "description": "Numbers of existing observations this one supersedes or consolidates.",
            "items": {"type": "integer"}
          }
        }
      }
    },
    "drop": {
      "type": "array",
      "description": "Numbers of existing observations to remove outright.",
      "items": {"type": "integer"}
    }
  }
}`

// ObservationAge is how old an observation is, for `apex profile`.
func ObservationAge(o contextfs.Observation, now time.Time) string {
	if o.Date.IsZero() {
		return "undated"
	}
	days := int(now.Sub(o.Date).Hours() / 24)
	switch {
	case days <= 0:
		return "today"
	case days == 1:
		return "yesterday"
	case days < 30:
		return fmt.Sprintf("%d days ago", days)
	default:
		return o.Date.Format(contextfs.ObservationDateLayout)
	}
}
