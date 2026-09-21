package contextfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const canonicalProjectDoc = `---
name: ScholarRAG
status: active
stack: [python, fastapi, postgres]
started: 2026-03-14
---

## What it is
Retrieval system over a personal corpus of academic papers.

## Where I'm stuck
Chunking strategy loses table context, so numeric questions fail.
`

func TestParseProjectDoc(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantFront   bool
		wantName    string
		wantStatus  string
		wantStack   []string
		wantStarted string
		wantLead    string
	}{
		{
			name:        "canonical",
			body:        canonicalProjectDoc,
			wantFront:   true,
			wantName:    "ScholarRAG",
			wantStatus:  "active",
			wantStack:   []string{"python", "fastapi", "postgres"},
			wantStarted: "2026-03-14",
			wantLead:    "Retrieval system over a personal corpus of academic papers.",
		},
		{
			name:      "no frontmatter",
			body:      "# Apex\n\nA personal hub.\n",
			wantFront: false,
			wantLead:  "A personal hub.",
		},
		{
			name:      "empty frontmatter",
			body:      "---\n---\n\nJust a body.\n",
			wantFront: true,
			wantLead:  "Just a body.",
		},
		{
			name:      "block sequence stack",
			body:      "---\nname: Apex\nstack:\n  - go\n  - sqlite\n---\nBody.\n",
			wantFront: true,
			wantName:  "Apex",
			wantStack: []string{"go", "sqlite"},
			wantLead:  "Body.",
		},
		{
			name:      "scalar stack becomes a list of one",
			body:      "---\nstack: go\n---\nBody.\n",
			wantFront: true,
			wantStack: []string{"go"},
			wantLead:  "Body.",
		},
		{
			name:       "quoted and null values",
			body:       "---\nname: \"Apex\"\nstatus:\n---\nBody.\n",
			wantFront:  true,
			wantName:   "Apex",
			wantStatus: "",
			wantLead:   "Body.",
		},
		{
			name:      "crlf frontmatter",
			body:      "---\r\nname: Apex\r\nstatus: active\r\n---\r\nBody line.\r\n",
			wantFront: true,
			wantName:  "Apex", wantStatus: "active",
			wantLead: "Body line.",
		},
		{
			name:      "closing delimiter is ...",
			body:      "---\nname: Apex\n...\nBody.\n",
			wantFront: true,
			wantName:  "Apex",
			wantLead:  "Body.",
		},
		{
			name:      "lead skips headings, lists, and fences",
			body:      "---\nname: Apex\n---\n\n## What it is\n\n```go\nfunc main() {}\n```\n\n- a bullet\n\nThe actual lead, which\nspans two lines.\n\nA second paragraph.\n",
			wantFront: true,
			wantName:  "Apex",
			wantLead:  "The actual lead, which spans two lines.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := ParseProjectDoc([]byte(tt.body), "PROJECT.md")
			if err != nil {
				t.Fatalf("ParseProjectDoc: %v", err)
			}
			if !d.Present {
				t.Error("Present is false for parsed bytes")
			}
			if d.Raw != tt.body {
				t.Error("Raw is not the input verbatim")
			}
			if (d.Front != nil) != tt.wantFront {
				t.Fatalf("Front != nil is %v, want %v", d.Front != nil, tt.wantFront)
			}
			if got := d.Name(); got != tt.wantName {
				t.Errorf("Name = %q, want %q", got, tt.wantName)
			}
			if got := d.Status(); got != tt.wantStatus {
				t.Errorf("Status = %q, want %q", got, tt.wantStatus)
			}
			if got := d.Stack(); !reflect.DeepEqual(got, tt.wantStack) {
				t.Errorf("Stack = %v, want %v", got, tt.wantStack)
			}
			if tt.wantStarted != "" && d.Front != nil && d.Front.Started != tt.wantStarted {
				// A YAML timestamp must survive as the text the user typed.
				t.Errorf("Started = %q, want %q", d.Front.Started, tt.wantStarted)
			}
			if got := d.Lead(); got != tt.wantLead {
				t.Errorf("Lead = %q, want %q", got, tt.wantLead)
			}
		})
	}
}

// TestProjectDocPreservesUnknownKeys is the guarantee that matters for a file
// the user owns: Apex not understanding a key is never a reason to drop it.
func TestProjectDocPreservesUnknownKeys(t *testing.T) {
	body := `---
name: ScholarRAG
# a comment the user wrote
tags: [research, nlp]
status: active
owner:
  name: Vedant
  email: v@example.com
priority: 3
archived: false
stack: [python]
---

## What it is
Some prose.
`

	d, err := ParseProjectDoc([]byte(body), "PROJECT.md")
	if err != nil {
		t.Fatalf("ParseProjectDoc: %v", err)
	}

	wantKeys := []string{"name", "tags", "status", "owner", "priority", "archived", "stack"}
	if got := d.Front.Keys(); !reflect.DeepEqual(got, wantKeys) {
		t.Errorf("Keys = %v, want %v (document order)", got, wantKeys)
	}

	for _, key := range []string{"tags", "owner", "priority", "archived"} {
		if _, ok := d.Front.Extra[key]; !ok {
			t.Errorf("unknown key %q is missing from Extra: %v", key, d.Front.Extra)
		}
	}
	if _, ok := d.Front.Extra["name"]; ok {
		t.Error("a known key leaked into Extra")
	}
	if got, ok := d.Front.Extra["priority"].(int); !ok || got != 3 {
		t.Errorf("Extra[priority] = %#v, want int 3", d.Front.Extra["priority"])
	}
	if got, ok := d.Front.Extra["archived"].(bool); !ok || got {
		t.Errorf("Extra[archived] = %#v, want bool false", d.Front.Extra["archived"])
	}

	out, err := d.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	rendered := string(out)
	for _, want := range []string{
		"tags:", "research", "nlp", "owner:", "v@example.com",
		"priority: 3", "archived: false", "# a comment the user wrote",
		"## What it is", "Some prose.",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered document lost %q:\n%s", want, rendered)
		}
	}

	// Rendering must be a fixed point: parse, render, parse again, render
	// again produces the same bytes, so a rewrite cannot drift.
	again, err := ParseProjectDoc(out, "PROJECT.md")
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	out2, err := again.Bytes()
	if err != nil {
		t.Fatalf("second Bytes: %v", err)
	}
	if string(out2) != rendered {
		t.Errorf("render is not a fixed point:\nfirst:\n%s\nsecond:\n%s", rendered, out2)
	}
	if !reflect.DeepEqual(again.Front.Keys(), wantKeys) {
		t.Errorf("keys after a round trip = %v, want %v", again.Front.Keys(), wantKeys)
	}
}

func TestParseProjectDocMalformed(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unterminated frontmatter",
			body: "---\nname: Apex\n\n## What it is\nProse.\n",
			want: "never closed",
		},
		{
			name: "invalid yaml",
			body: "---\nname: [unclosed\n---\nBody.\n",
			want: "invalid YAML",
		},
		{
			name: "frontmatter is not a mapping",
			body: "---\n- one\n- two\n---\nBody.\n",
			want: "must be a mapping",
		},
		{
			name: "known key is not a scalar",
			body: "---\nstatus: [a, b]\n---\nBody.\n",
			want: "must be a single value",
		},
		{
			name: "stack is a mapping",
			body: "---\nstack:\n  a: b\n---\nBody.\n",
			want: "must be a value or a list",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseProjectDoc([]byte(tt.body), "PROJECT.md")
			if err == nil {
				t.Fatal("ParseProjectDoc accepted a malformed document")
			}
			var malformed *MalformedError
			if !errors.As(err, &malformed) {
				t.Fatalf("err = %T (%v), want *MalformedError", err, err)
			}
			if !malformed.UserFixable() {
				t.Error("a malformed PROJECT.md should be user-fixable")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %q, want it to mention %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), "PROJECT.md") {
				t.Errorf("err = %q, want it to name the file", err)
			}
		})
	}
}

func TestLoadProjectDocAbsentAndPresent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	d, err := LoadProjectDoc(ctx, dir)
	if err != nil {
		t.Fatalf("LoadProjectDoc on a directory without one: %v", err)
	}
	if d.Present {
		t.Error("Present is true for a project with no PROJECT.md")
	}
	if d.Name() != "" || d.Status() != "" || d.Lead() != "" {
		t.Errorf("an absent document reported content: %+v", d)
	}
	if d.Path != filepath.Join(dir, ProjectFile) {
		t.Errorf("Path = %q", d.Path)
	}

	if err := os.WriteFile(filepath.Join(dir, ProjectFile), []byte(canonicalProjectDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err = LoadProjectDoc(ctx, dir)
	if err != nil {
		t.Fatalf("LoadProjectDoc: %v", err)
	}
	if !d.Present || d.Name() != "ScholarRAG" {
		t.Errorf("loaded document = %+v", d)
	}
}

func TestDocumentPresentVersusEmpty(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(ContextDir(root), 0o700); err != nil {
		t.Fatal(err)
	}

	// Absent: M4 must be able to tell this from a file that exists and is
	// empty, so the two states are reported separately.
	id, err := LoadIdentity(ctx, root)
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	if id.Profile.Present || id.Skills.Present {
		t.Error("Present is true for files that do not exist")
	}
	if !id.Empty() {
		t.Error("Empty is false with neither document present")
	}

	if err := os.WriteFile(ProfilePath(root), []byte("   \n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SkillsPath(root), []byte("Go, SQL.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err = LoadIdentity(ctx, root)
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	if !id.Profile.Present {
		t.Error("Present is false for a file that exists but is blank")
	}
	if !id.Profile.Empty() {
		t.Error("Empty is false for a blank file")
	}
	if !id.Skills.Present || id.Skills.Empty() || id.Skills.Body != "Go, SQL.\n" {
		t.Errorf("skills = %+v", id.Skills)
	}
	if id.Empty() {
		t.Error("Empty is true with SKILLS.md present")
	}
}

func TestPathHelpers(t *testing.T) {
	root := filepath.Join("home", ".apex")
	tests := []struct {
		got  string
		want string
	}{
		{ContextDir(root), filepath.Join(root, "context")},
		{ProfilePath(root), filepath.Join(root, "context", ProfileFile)},
		{SkillsPath(root), filepath.Join(root, "context", SkillsFile)},
		{RegistryPath(root), filepath.Join(root, "context", ProjectsFile)},
		{ProjectDocPath("/p"), filepath.Join("/p", ProjectFile)},
		{ReadmePath("/p"), filepath.Join("/p", ReadmeFile)},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("got %q, want %q", tt.got, tt.want)
		}
	}
}
