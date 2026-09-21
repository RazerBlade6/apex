package contextfs

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// PROJECT.md lives in the project's own directory and is committed to its repo
// (DESIGN.md §6): YAML frontmatter plus a markdown body.
//
//	---
//	name: ScholarRAG
//	status: active
//	stack: [python, fastapi, postgres]
//	started: 2026-03-14
//	---
//
//	## What it is
//	...
//
// The user owns this file, so every frontmatter key Apex does not recognise is
// kept, in its original order and with its comments, and written back
// unchanged. Apex reading a file must never be a reason for the file to lose
// something the user put in it.

// Frontmatter is the parsed YAML header of a PROJECT.md.
type Frontmatter struct {
	Name    string
	Status  string
	Stack   []string
	Started string

	// Extra holds every key Apex does not model, decoded as plain Go values.
	// It is a view: Bytes renders from the original node, so unknown keys
	// survive with their formatting and comments.
	Extra map[string]any

	// keys is every key in document order, known and unknown alike.
	keys []string
	// node is the mapping node the frontmatter was decoded from, kept so it
	// can be re-encoded without loss.
	node *yaml.Node
}

// Keys returns every frontmatter key in document order.
func (f *Frontmatter) Keys() []string {
	if f == nil {
		return nil
	}
	out := make([]string, len(f.keys))
	copy(out, f.keys)
	return out
}

// ProjectDoc is a parsed PROJECT.md.
type ProjectDoc struct {
	// Path is the file the document was read from.
	Path string
	// Present is false when the project has no PROJECT.md. That is a valid
	// state — a project can be registered before it is described.
	Present bool
	// Raw is the file exactly as read. It is the input to the source hash, so
	// it is kept verbatim rather than reconstructed.
	Raw string
	// Body is everything after the frontmatter.
	Body string
	// Front is nil when the file has no frontmatter block.
	Front *Frontmatter
}

// LoadProjectDoc reads and parses <dir>/PROJECT.md. A project without one
// yields Present false and no error; a project with a malformed one yields a
// *MalformedError, which is user-fixable.
func LoadProjectDoc(ctx context.Context, dir string) (*ProjectDoc, error) {
	path := ProjectDocPath(dir)
	doc, err := LoadDocument(ctx, path)
	if err != nil {
		return nil, err
	}
	if !doc.Present {
		return &ProjectDoc{Path: path}, nil
	}
	return ParseProjectDoc([]byte(doc.Body), path)
}

// ParseProjectDoc parses PROJECT.md bytes that are already in hand.
func ParseProjectDoc(data []byte, path string) (*ProjectDoc, error) {
	d := &ProjectDoc{Path: path, Present: true, Raw: string(data)}

	front, body, ok, err := splitFrontmatter(data, path)
	if err != nil {
		return nil, err
	}
	d.Body = body
	if !ok {
		// No frontmatter is not malformed: a plain markdown PROJECT.md is
		// still a description of the project.
		return d, nil
	}

	fm, err := parseFrontmatter(front, path)
	if err != nil {
		return nil, err
	}
	d.Front = fm
	return d, nil
}

// splitFrontmatter separates a leading `---` block from the body. It reports ok
// false when the file does not open with a delimiter, and an error when it
// opens with one that is never closed — an unterminated block means the whole
// file was meant as frontmatter and none of it can be trusted.
func splitFrontmatter(data []byte, path string) (front, body string, ok bool, err error) {
	lines := splitLines(data)
	if len(lines) == 0 {
		return "", "", false, nil
	}
	if !isFrontmatterDelimiter(lineText(lines[0])) {
		return "", string(data), false, nil
	}
	for i := 1; i < len(lines); i++ {
		text := lineText(lines[i])
		if !isFrontmatterDelimiter(text) && text != "..." {
			continue
		}
		return string(joinLines(lines[1:i])), string(joinLines(lines[i+1:])), true, nil
	}
	return "", "", false, &MalformedError{
		Path: path,
		Line: 1,
		Msg:  "frontmatter opens with `---` but is never closed",
	}
}

func isFrontmatterDelimiter(text string) bool {
	return strings.TrimRight(text, " \t") == "---"
}

// parseFrontmatter decodes the YAML header, pulling out the keys Apex models
// and retaining everything else.
func parseFrontmatter(front, path string) (*Frontmatter, error) {
	fm := &Frontmatter{Extra: map[string]any{}}

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(front), &doc); err != nil {
		return nil, &MalformedError{Path: path, Line: 2, Msg: "invalid YAML frontmatter", Err: err}
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return fm, nil // an empty block is valid, just uninformative
	}
	mapping := doc.Content[0]
	if mapping.Kind != yaml.MappingNode {
		return nil, &MalformedError{
			Path: path,
			Line: mapping.Line + 1,
			Msg:  "frontmatter must be a mapping of keys to values",
		}
	}
	fm.node = mapping

	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key, value := mapping.Content[i], mapping.Content[i+1]
		fm.keys = append(fm.keys, key.Value)

		switch strings.ToLower(key.Value) {
		case "name":
			s, err := scalar(key.Value, value, path)
			if err != nil {
				return nil, err
			}
			fm.Name = s
		case "status":
			s, err := scalar(key.Value, value, path)
			if err != nil {
				return nil, err
			}
			fm.Status = s
		case "started":
			s, err := scalar(key.Value, value, path)
			if err != nil {
				return nil, err
			}
			fm.Started = s
		case "stack":
			stack, err := stringList(key.Value, value, path)
			if err != nil {
				return nil, err
			}
			fm.Stack = stack
		default:
			var v any
			if err := value.Decode(&v); err != nil {
				return nil, &MalformedError{
					Path: path,
					Line: value.Line + 1,
					Msg:  fmt.Sprintf("cannot read frontmatter key %q", key.Value),
					Err:  err,
				}
			}
			fm.Extra[key.Value] = v
		}
	}
	return fm, nil
}

// scalar reads a scalar frontmatter value as text. Reading node.Value directly
// rather than decoding into a string keeps a date like 2026-03-14 as the text
// the user typed, instead of letting the YAML timestamp resolver turn it into
// something else.
func scalar(key string, node *yaml.Node, path string) (string, error) {
	if node.Kind != yaml.ScalarNode {
		return "", &MalformedError{
			Path: path,
			Line: node.Line + 1,
			Msg:  fmt.Sprintf("frontmatter key %q must be a single value", key),
		}
	}
	if node.Tag == "!!null" {
		return "", nil
	}
	return node.Value, nil
}

// stringList reads `stack:` as a list, accepting a bare scalar as a list of one
// so `stack: go` does not have to be written `stack: [go]`.
func stringList(key string, node *yaml.Node, path string) ([]string, error) {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag == "!!null" || strings.TrimSpace(node.Value) == "" {
			return nil, nil
		}
		return []string{node.Value}, nil
	case yaml.SequenceNode:
		out := make([]string, 0, len(node.Content))
		for _, item := range node.Content {
			s, err := scalar(key, item, path)
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, &MalformedError{
			Path: path,
			Line: node.Line + 1,
			Msg:  fmt.Sprintf("frontmatter key %q must be a value or a list", key),
		}
	}
}

// Bytes renders the document back to markdown. Frontmatter is re-encoded from
// the node it was parsed into, so every key — including ones Apex does not
// model — keeps its position, its comments, and its value. The body is passed
// through untouched.
func (d *ProjectDoc) Bytes() ([]byte, error) {
	if d.Front == nil || d.Front.node == nil {
		return []byte(d.Body), nil
	}

	var yamlBuf bytes.Buffer
	enc := yaml.NewEncoder(&yamlBuf)
	enc.SetIndent(2)
	if err := enc.Encode(d.Front.node); err != nil {
		return nil, fmt.Errorf("encode frontmatter for %s: %w", d.Path, err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode frontmatter for %s: %w", d.Path, err)
	}

	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.Write(yamlBuf.Bytes())
	buf.WriteString("---\n")
	buf.WriteString(d.Body)
	return buf.Bytes(), nil
}

// Name returns the project name the document declares, or "" when it declares
// none.
func (d *ProjectDoc) Name() string {
	if d.Front == nil {
		return ""
	}
	return d.Front.Name
}

// Status returns the declared status, or "".
func (d *ProjectDoc) Status() string {
	if d.Front == nil {
		return ""
	}
	return d.Front.Status
}

// Stack returns the declared stack, or nil.
func (d *ProjectDoc) Stack() []string {
	if d.Front == nil {
		return nil
	}
	return d.Front.Stack
}

// Lead returns the first paragraph of prose in the body, folded onto one line.
// Headings, lists, block quotes, fenced code, and HTML comments are skipped:
// the lead is the sentence a person would use to say what the project is.
func (d *ProjectDoc) Lead() string {
	if d == nil {
		return ""
	}
	var para []string
	inFence := false
	for _, raw := range splitLines([]byte(d.Body)) {
		text := strings.TrimSpace(lineText(raw))
		if isFence(text) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if text == "" {
			if len(para) > 0 {
				break
			}
			continue
		}
		if strings.HasPrefix(text, "#") || strings.HasPrefix(text, "<!--") ||
			strings.HasPrefix(text, "-") || strings.HasPrefix(text, "*") ||
			strings.HasPrefix(text, "+") || strings.HasPrefix(text, ">") ||
			strings.HasPrefix(text, "|") {
			if len(para) > 0 {
				break
			}
			continue
		}
		para = append(para, text)
	}
	return strings.Join(para, " ")
}
