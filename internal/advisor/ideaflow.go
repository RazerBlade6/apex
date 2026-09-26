package advisor

import (
	"context"
	"fmt"
	"strings"

	"github.com/RazerBlade6/apex/internal/provider"
	"github.com/RazerBlade6/apex/internal/store"
)

// Project ideas from the chat (DESIGN.md §12).
//
// `apex ideas` records every idea it proposes and leaves the user to run `apex
// start` on one. The chat works the other way round, because a conversation is
// where the choosing happens: it proposes a list and records nothing, the user
// picks one, Apex describes it in more detail and asks a few questions whose
// answers shape it, and only when the user says yes is anything written — the
// idea, the project it becomes, and an action item to scaffold it.
//
// Three calls, each Structured, because each result drives the interface
// rather than being read: ProposeIdeas for `/ideas` (a reply can also carry an
// apex-ideas block), ExploreIdea for the detail and the questionnaire, and
// PlanIdea for the project and its first action item once the answers are in.

// IdeaFence is the info string of the fenced block a chat reply carries
// proposed project ideas in.
const IdeaFence = "apex-ideas"

// maxChatIdeas bounds one list, as maxChatProposals bounds a checklist.
const maxChatIdeas = 5

// capIdeas drops untitled ideas and bounds the rest.
func capIdeas(ideas []ProposedIdea) []ProposedIdea {
	var out []ProposedIdea
	for _, it := range ideas {
		if strings.TrimSpace(it.Title) == "" {
			continue
		}
		out = append(out, it)
		if len(out) == maxChatIdeas {
			break
		}
	}
	return out
}

// IdeaProposals is what ProposeIdeas produced.
type IdeaProposals struct {
	Ideas []ProposedIdea
	Usage provider.Usage
	Model string
}

// ProposeIdeas asks for new project ideas in the light of the conversation so
// far, which may be empty. Nothing is recorded.
func (c *Chat) ProposeIdeas(ctx context.Context) (*IdeaProposals, error) {
	existing, err := c.a.Store.ListIdeas(ctx, "")
	if err != nil {
		return nil, err
	}
	instruction := fmt.Sprintf(ideasInstructions, maxChatIdeas, renderOpenIdeas(existing))
	var decoded ideasResponse
	usage, model, err := c.structured(ctx, instruction, ideasSchema, &decoded)
	if err != nil {
		return nil, err
	}
	return &IdeaProposals{Ideas: capIdeas(decoded.Ideas), Usage: usage, Model: model}, nil
}

// IdeaBrief is an idea described in more depth, with the questions whose
// answers would shape it.
type IdeaBrief struct {
	// Details is markdown: what the project would look like, a suggested
	// stack, the first milestone, the risks.
	Details   string         `json:"details"`
	Questions []string       `json:"questions"`
	Usage     provider.Usage `json:"-"`
	Model     string         `json:"-"`
}

// maxIdeaQuestions bounds the questionnaire. It is meant to be answered in a
// minute, not to be a requirements document.
const maxIdeaQuestions = 5

const exploreInstructions = `The user picked this project idea to pursue:

## %s

%s

Why it fits them: %s

Describe it in more depth, and ask a short questionnaire.

- "details" is markdown, a few short paragraphs or bullets: what the first
  version would do, a stack you would suggest given what they know, the first
  milestone, and the biggest risk. Concrete, not a sales pitch.
- "questions" are 3 to %d short questions whose answers would change how the
  project is built: scope of the first milestone, stack or language, where it
  runs, who it is for, what "working" means. One line each, answerable in a
  sentence. Do not ask what the context above already answers.`

const exploreSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["details", "questions"],
  "properties": {
    "details": {
      "type": "string",
      "description": "Markdown: the first version, a suggested stack, the first milestone, the biggest risk."
    },
    "questions": {
      "type": "array",
      "description": "Short questions whose answers shape how the project is built.",
      "items": {"type": "string"}
    }
  }
}`

// ExploreIdea describes one idea in depth and asks the questionnaire. Nothing
// is recorded, and the call is not a turn in the conversation; the caller adds
// what the user saw with Note.
func (c *Chat) ExploreIdea(ctx context.Context, idea ProposedIdea) (*IdeaBrief, error) {
	instruction := fmt.Sprintf(exploreInstructions,
		strings.TrimSpace(idea.Title), strings.TrimSpace(idea.Pitch), strings.TrimSpace(idea.Rationale),
		maxIdeaQuestions)
	var brief IdeaBrief
	usage, model, err := c.structured(ctx, instruction, exploreSchema, &brief)
	if err != nil {
		return nil, err
	}
	var qs []string
	for _, q := range brief.Questions {
		if q = strings.TrimSpace(q); q != "" {
			qs = append(qs, q)
		}
		if len(qs) == maxIdeaQuestions {
			break
		}
	}
	brief.Questions, brief.Usage, brief.Model = qs, usage, model
	return &brief, nil
}

// Answer is one questionnaire question and what the user said. An empty
// Answer is a question the user skipped.
type Answer struct {
	Question string
	Answer   string
}

// IdeaPlan is a settled idea: the project it becomes, and the action item that
// scaffolds it.
type IdeaPlan struct {
	// Name is the project's name: its PROJECTS.md heading and, by default,
	// its directory.
	Name      string `json:"name"`
	Pitch     string `json:"pitch"`
	Rationale string `json:"rationale"`
	// Stack is a one-word stack, used to pick an ecosystem scaffolder.
	Stack string       `json:"stack"`
	Item  ProposedItem `json:"item"`
	// Source is the idea as it was picked, before the answers refined it.
	Source ProposedIdea   `json:"-"`
	Model  string         `json:"-"`
	Usage  provider.Usage `json:"-"`
}

const planInstructions = `The user has settled on this project idea and wants it made real:

## %s

%s

Why it fits them: %s

Their answers to your questions:
%s

Write it up as a project and the first action item on it.

- "name" is the project's name: short, a proper noun, no punctuation beyond
  spaces and hyphens. It becomes a directory name and a heading.
- "pitch" is what it is, in markdown, updated with the answers. End with the
  first milestone.
- "rationale" is why it fits this user, in their terms.
- "stack" is one lowercase word for the main language or ecosystem the answers
  point to (go, rust, python, node, …), or "" if nothing is settled.
- "item" is the action item a coding agent will be dispatched with, in an
  empty directory that already has git, a PROJECT.md and a .gitignore:
  "title" is one imperative line; "body" says what to build for the first
  milestone and nothing more, with acceptance criteria, reflecting the
  answers; "rationale" is why this is the first step; "effort" is small,
  medium or large; "project" is the same as "name".`

const planSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["name", "pitch", "rationale", "stack", "item"],
  "properties": {
    "name": {"type": "string", "description": "The project's name."},
    "pitch": {"type": "string", "description": "Markdown: what it is, ending with the first milestone."},
    "rationale": {"type": "string", "description": "Why it fits this user."},
    "stack": {"type": "string", "description": "One lowercase word, or empty."},
    "item": {
      "type": "object",
      "additionalProperties": false,
      "required": ["project", "title", "body", "rationale", "effort"],
      "properties": {
        "project": {"type": "string"},
        "title": {"type": "string", "description": "One imperative line."},
        "body": {"type": "string", "description": "Markdown, with acceptance criteria."},
        "rationale": {"type": "string"},
        "effort": {"type": "string", "enum": ["small", "medium", "large"]}
      }
    }
  }
}`

// PlanIdea turns a picked idea and the user's answers into a project and its
// first action item. Nothing is recorded: the caller creates the project,
// because that is filesystem work this package does not do.
func (c *Chat) PlanIdea(ctx context.Context, idea ProposedIdea, answers []Answer) (*IdeaPlan, error) {
	var qa strings.Builder
	for _, a := range answers {
		ans := strings.TrimSpace(a.Answer)
		if ans == "" {
			ans = "(skipped — use your judgment)"
		}
		fmt.Fprintf(&qa, "- %s\n  %s\n", a.Question, ans)
	}
	if qa.Len() == 0 {
		qa.WriteString("(none asked)\n")
	}
	instruction := fmt.Sprintf(planInstructions,
		strings.TrimSpace(idea.Title), strings.TrimSpace(idea.Pitch), strings.TrimSpace(idea.Rationale),
		strings.TrimRight(qa.String(), "\n"))

	var plan IdeaPlan
	usage, model, err := c.structured(ctx, instruction, planSchema, &plan)
	if err != nil {
		return nil, err
	}
	plan.Name = strings.TrimSpace(plan.Name)
	if plan.Name == "" {
		plan.Name = strings.TrimSpace(idea.Title)
	}
	if strings.TrimSpace(plan.Pitch) == "" {
		plan.Pitch = idea.Pitch
	}
	if strings.TrimSpace(plan.Rationale) == "" {
		plan.Rationale = idea.Rationale
	}
	if strings.TrimSpace(plan.Item.Title) == "" {
		plan.Item.Title = "Scaffold " + plan.Name + " so the first milestone can begin"
	}
	plan.Item.Project = plan.Name
	plan.Item.Effort = normalizeEffort(plan.Item.Effort)
	plan.Stack = strings.ToLower(strings.TrimSpace(plan.Stack))
	plan.Source, plan.Model, plan.Usage = idea, model, usage
	return &plan, nil
}

// Note adds a turn to the conversation and the transcript without calling a
// model: what the user saw and said during a questionnaire, so the rest of the
// conversation knows it happened.
func (c *Chat) Note(ctx context.Context, role, content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	if err := c.ensureSession(ctx, content); err != nil {
		return err
	}
	pr := provider.RoleUser
	if role == store.RoleAssistant {
		pr = provider.RoleAssistant
	}
	c.History = append(c.History, provider.Message{Role: pr, Content: content})
	return c.append(ctx, role, content, "", nil)
}

// structured is one Structured call on the chat route, over the conversation
// so far, not added to it.
func (c *Chat) structured(ctx context.Context, instruction, schemaText string, out any) (provider.Usage, string, error) {
	schema, err := schemaBytes(schemaText)
	if err != nil {
		return provider.Usage{}, "", err
	}
	route := c.a.Config.Models.Chat
	p, err := c.a.providerFor(ctx, route)
	if err != nil {
		return provider.Usage{}, "", err
	}
	prompt, err := c.prompt(ctx, instruction)
	if err != nil {
		return provider.Usage{}, "", err
	}
	prompt.History = c.window()
	req := prompt.Build(route.Model, route.Effort, listMaxTokens)
	usage, err := p.Structured(ctx, req, schema, out)
	if err != nil {
		return usage, "", err
	}
	model := usage.Model
	if model == "" {
		model = route.Model
	}
	return usage, model, nil
}
