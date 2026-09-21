package advisor

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// The schemas below are written to the intersection of what every provider
// accepts (DESIGN.md §8): an object at the root, every property listed in
// `required`, and `additionalProperties: false` everywhere. OpenAI's strict
// mode enforces exactly that set, Anthropic's output_config.format is happy
// with it, and the claude CLI's --json-schema is the same validator.
//
// Writing to the intersection from the start is the point. A schema only one
// vendor accepts turns a one-line config change into a runtime 400, and on a
// machine routing every slot at claude-cli that 400 would arrive as a failed
// subprocess rather than as anything obviously schema-shaped.
//
// They are string constants rather than structs built at runtime because a
// schema is a literal, and TestSchemasAreStrictCompatible checks these exact
// bytes.

// actionItemsSchema constrains `apex review` output.
//
// `project` is a plain string: an enum of the current slugs would be tighter,
// but it would have to be rebuilt per call, which changes the schema bytes on
// every sync and is one more thing that can drift out of the cache prefix.
// The slug is resolved and validated in Go instead, where an unknown project
// can be reported to the user rather than rejected by the vendor.
const actionItemsSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["items"],
  "properties": {
    "items": {
      "type": "array",
      "description": "Action items, most important first.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["project", "title", "body", "rationale", "effort"],
        "properties": {
          "project": {
            "type": "string",
            "description": "The slug of the project this item belongs to, exactly as given in the portfolio."
          },
          "title": {
            "type": "string",
            "description": "One imperative line, under 80 characters."
          },
          "body": {
            "type": "string",
            "description": "What to do, concretely, in markdown. Include acceptance criteria."
          },
          "rationale": {
            "type": "string",
            "description": "Why this matters now, grounded in the digest."
          },
          "effort": {
            "type": "string",
            "enum": ["small", "medium", "large"]
          }
        }
      }
    }
  }
}`

// ideasSchema constrains `apex ideas` output. It has no project field: an
// idea is a project that does not exist yet.
const ideasSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["ideas"],
  "properties": {
    "ideas": {
      "type": "array",
      "description": "Project proposals, strongest fit first.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["title", "pitch", "rationale"],
        "properties": {
          "title": {
            "type": "string",
            "description": "A short project name."
          },
          "pitch": {
            "type": "string",
            "description": "What it is and what it would do, in markdown. A few sentences, plus a first milestone."
          },
          "rationale": {
            "type": "string",
            "description": "Why this fits this user specifically, grounded in their profile, skills, and existing projects."
          }
        }
      }
    }
  }
}`

// schemaBytes returns a schema as json.RawMessage, failing loudly on a schema
// this package itself got wrong.
func schemaBytes(s string) (json.RawMessage, error) {
	var probe map[string]any
	if err := json.Unmarshal([]byte(s), &probe); err != nil {
		return nil, fmt.Errorf("advisor: built-in schema is not valid JSON: %w", err)
	}
	return json.RawMessage(s), nil
}

// normalizeTitle reduces a title to the form deduplication compares.
//
// Lowercase, punctuation dropped, whitespace collapsed. It is deliberately
// crude: the job is to catch the model proposing "Add table-aware chunking"
// in a week when "Add table aware chunking." is already open, not to detect
// paraphrase. Anything cleverer here would need embeddings, which DESIGN.md
// §1 rules out of v1, and a false merge is worse than a duplicate — a
// duplicate the user can dismiss, a silently dropped item they never see.
func normalizeTitle(s string) string {
	var b strings.Builder
	space := true
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			space = false
		case !space:
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}
