package advisor

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/RazerBlade6/apex/internal/contextfs"
)

// writeSkills creates a SKILLS.md, which newHarness does not.
func (h *harness) writeSkills(body string) {
	h.T.Helper()
	if err := os.WriteFile(contextfs.SkillsPath(h.Root), []byte(body), 0o600); err != nil {
		h.T.Fatal(err)
	}
}

func (h *harness) observations() *Observations {
	h.T.Helper()
	obs, err := LoadObservations(context.Background(), h.Root)
	if err != nil {
		h.T.Fatal(err)
	}
	return obs
}

// TestScreenTurnGate is DESIGN.md §6's "extraction is gated, not per-turn".
//
// The cost argument only holds if the gate actually rejects ordinary
// conversation, which is most of it. It errs wide deliberately: a false
// positive costs one cheap call that returns nothing, a false negative loses a
// fact silently.
func TestScreenTurnGate(t *testing.T) {
	fire := []string{
		"I always build CLIs in Go or Rust.",
		"I'm learning Zig at the moment.",
		"honestly I never want to touch Kubernetes again",
		"My preference is a single static binary.",
		"I know Postgres well enough, I've used it for years.",
	}
	quiet := []string{
		"What does apex sync actually do?",
		"Summarise the design document.",
		"Which project is furthest along?",
		"Show me AI-003.",
		"always run the tests before committing", // an instruction, not a self-description
		"",
	}

	for _, s := range fire {
		if !ScreenTurn(s) {
			t.Errorf("gate rejected a turn that says something durable: %q", s)
		}
	}
	for _, s := range quiet {
		if ScreenTurn(s) {
			t.Errorf("gate fired on a turn that reveals nothing, which is the cost this gate exists to avoid: %q", s)
		}
	}
}

// TestObserveSpendsNothingOnAnUnrevealingTurn is the same rule, checked where
// it costs money rather than where it is decided.
func TestObserveSpendsNothingOnAnUnrevealingTurn(t *testing.T) {
	fake := &fakeProvider{JSON: `{"observations":[],"drop":[]}`}
	h := newHarness(t, fake)
	obs := h.observations()

	result, err := h.Observe(context.Background(), obs, Turn{
		User:      "Which of my projects is closest to done?",
		Assistant: "ScholarRAG.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Screened || result.Called {
		t.Fatalf("result = %+v, want the gate to have stopped before any call", result)
	}
	if fake.Calls() != 0 {
		t.Fatalf("the provider was called %d time(s) for a turn revealing nothing", fake.Calls())
	}
}

func TestObserveRecordsClaimSourceAndFile(t *testing.T) {
	fake := &fakeProvider{JSON: `{
      "observations": [
        {"file":"skills","claim":"Is learning Zig.","source":"said while asking about a new project","replaces":[]},
        {"file":"profile","claim":"Prefers a single static binary.","source":"said while choosing a stack","replaces":[]}
      ],
      "drop": []
    }`}
	h := newHarness(t, fake)
	h.writeIdentity("# Me\n\nI build small tools.\n")
	h.writeSkills("# Skills\n\nGo, SQL.\n")
	obs := h.observations()

	result, err := h.Observe(context.Background(), obs, Turn{
		User: "I'm learning Zig and I prefer shipping a single static binary.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Screened || !result.Called {
		t.Fatalf("result = %+v, want the gate to have passed", result)
	}
	if len(result.Added) != 2 {
		t.Fatalf("added %d observation(s), want 2", len(result.Added))
	}

	reloaded := h.observations()
	skills := reloaded.Skills.Observations()
	if len(skills) != 1 || skills[0].Claim != "Is learning Zig." {
		t.Fatalf("SKILLS.md observations = %+v", skills)
	}
	if skills[0].Source == "" {
		t.Error("the source was dropped; a wrong inference must be debuggable rather than mysterious")
	}
	profile := reloaded.Profile.Observations()
	if len(profile) != 1 || profile[0].Claim != "Prefers a single static binary." {
		t.Fatalf("PROFILE.md observations = %+v", profile)
	}
	// The marker carries a date, not a timestamp, so what survives the round
	// trip is the day the advisor clock was on.
	if got, want := profile[0].Date.Format(contextfs.ObservationDateLayout),
		fixedNow.Format(contextfs.ObservationDateLayout); got != want {
		t.Errorf("date = %s, want %s from the advisor clock", got, want)
	}

	// And the user's own prose is untouched, which is the whole contract.
	body, err := os.ReadFile(contextfs.ProfilePath(h.Root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), "# Me\n\nI build small tools.\n") {
		t.Fatalf("hand-written PROFILE.md changed:\n%s", body)
	}
}

// TestObserveSupersedesRatherThanStacks is DESIGN.md §6's second rule: "I'm
// learning Zig" followed by "I know Zig well now" must replace, not append.
func TestObserveSupersedesRatherThanStacks(t *testing.T) {
	fake := &fakeProvider{JSON: `{
      "observations": [
        {"file":"skills","claim":"Knows Zig well.","source":"said months after starting to learn it","replaces":[1]}
      ],
      "drop": []
    }`}
	h := newHarness(t, fake)
	h.writeSkills("# Skills\n\n## Observed\n<!--apex 2026-06-01--> Is learning Zig. — source: a remark\n")
	obs := h.observations()
	if got := len(obs.List()); got != 1 {
		t.Fatalf("seeded %d observation(s), want 1", got)
	}

	result, err := h.Observe(context.Background(), obs, Turn{User: "I know Zig well now."})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Superseded) != 1 {
		t.Fatalf("superseded %d, want 1", len(result.Superseded))
	}

	after := h.observations().Skills.Observations()
	if len(after) != 1 {
		t.Fatalf("observations stacked instead of superseding: %+v", after)
	}
	if after[0].Claim != "Knows Zig well." {
		t.Errorf("claim = %q, want the newer one", after[0].Claim)
	}
}

func TestObserveDropsWhatTheUserHasPromoted(t *testing.T) {
	fake := &fakeProvider{JSON: `{"observations":[],"drop":[1]}`}
	h := newHarness(t, fake)
	h.writeIdentity("# Me\n\nI build CLIs in Go.\n\n## Observed\n" +
		"<!--apex 2026-06-01--> Builds CLIs in Go. — source: a remark\n")
	obs := h.observations()

	result, err := h.Observe(context.Background(), obs, Turn{
		User: "I've written that into my profile, I build CLIs in Go.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Superseded) != 1 {
		t.Fatalf("dropped %d, want 1", len(result.Superseded))
	}
	if got := h.observations().Profile.Observations(); len(got) != 0 {
		t.Fatalf("observations = %+v, want none left", got)
	}
	body, _ := os.ReadFile(contextfs.ProfilePath(h.Root))
	if !strings.Contains(string(body), "I build CLIs in Go.") {
		t.Fatalf("dropping an observation took the user's own sentence with it:\n%s", body)
	}
}

// TestObservePromptCarriesTheNumbersItAsksFor: supersession is expressed as
// indices, so the prompt has to actually show them.
func TestObservePromptCarriesTheNumbersItAsksFor(t *testing.T) {
	fake := &fakeProvider{JSON: `{"observations":[],"drop":[]}`}
	h := newHarness(t, fake)
	h.writeIdentity("# Me\n\nMy own prose.\n\n## Observed\n" +
		"<!--apex 2026-06-01--> First claim. — source: a\n")
	h.writeSkills("# Skills\n\n## Observed\n<!--apex 2026-06-02--> Second claim. — source: b\n")

	if _, err := h.Observe(context.Background(), h.observations(), Turn{
		User: "I always work this way.",
	}); err != nil {
		t.Fatal(err)
	}
	if fake.Calls() != 1 {
		t.Fatalf("calls = %d, want 1", fake.Calls())
	}
	system := fake.Requests[0].System
	for _, want := range []string{"1. [profile] First claim.", "2. [skills] Second claim.", "My own prose."} {
		if !strings.Contains(system, want) {
			t.Errorf("the extraction prompt is missing %q:\n%s", want, system)
		}
	}
}

func TestForgetRemovesOneObservationByNumber(t *testing.T) {
	h := newHarness(t, &fakeProvider{})
	h.writeIdentity("# Me\n\n## Observed\n" +
		"<!--apex 2026-06-01--> First. — source: a\n" +
		"<!--apex 2026-06-02--> Second. — source: b\n")
	h.writeSkills("# Skills\n\n## Observed\n<!--apex 2026-06-03--> Third. — source: c\n")

	obs := h.observations()
	if _, err := obs.Forget(4); err == nil {
		t.Error("Forget accepted a number that names nothing")
	}
	entry, err := obs.Forget(2)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Obs.Claim != "Second." {
		t.Fatalf("forgot %q, want the second", entry.Obs.Claim)
	}
	if err := obs.Save(context.Background()); err != nil {
		t.Fatal(err)
	}

	list := h.observations().List()
	if len(list) != 2 {
		t.Fatalf("list = %+v, want 2 remaining", list)
	}
	if list[0].Obs.Claim != "First." || list[1].Obs.Claim != "Third." {
		t.Errorf("remaining = %q, %q", list[0].Obs.Claim, list[1].Obs.Claim)
	}
	if list[1].File != ObservationSkills {
		t.Errorf("numbering lost the file: %+v", list[1])
	}
}

// TestObservationsSchemaIsStrictCompatible mirrors the check the other
// schemas get: object root, every property required, additionalProperties
// false everywhere. A schema only one vendor accepts turns a config change
// into a runtime 400.
func TestObservationsSchemaIsStrictCompatible(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal([]byte(observationsSchema), &root); err != nil {
		t.Fatalf("the observations schema is not valid JSON: %v", err)
	}
	var walk func(path string, node map[string]any)
	walk = func(path string, node map[string]any) {
		if node["type"] != "object" {
			if props, ok := node["items"].(map[string]any); ok {
				walk(path+"[]", props)
			}
			return
		}
		if node["additionalProperties"] != false {
			t.Errorf("%s: additionalProperties must be false", path)
		}
		props, _ := node["properties"].(map[string]any)
		required, _ := node["required"].([]any)
		if len(required) != len(props) {
			t.Errorf("%s: %d required, %d properties; every property must be required",
				path, len(required), len(props))
		}
		for name, raw := range props {
			child, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			walk(path+"."+name, child)
		}
	}
	walk("root", root)
}
