// Package contextfs parses the markdown files that carry Apex's context
// (DESIGN.md §6): the identity documents PROFILE.md and SKILLS.md, the project
// registry PROJECTS.md, and the per-project PROJECT.md.
//
// Two rules shape everything here.
//
// Absence is not an error. A missing PROFILE.md means "no identity context
// yet", which a caller assembling a prompt must be able to tell apart from a
// file that exists and is empty. Every loader therefore reports Present
// separately from Body.
//
// The user owns these files. PROJECTS.md in particular is hand-edited and
// machine-rewritten on every sync, so parsing preserves every byte it does not
// deliberately change, writing is atomic, and an entry Apex cannot parse is
// reported and left alone rather than dropped.
package contextfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// File names under the Apex context directory and inside a project.
const (
	ProfileFile  = "PROFILE.md"
	SkillsFile   = "SKILLS.md"
	ProjectsFile = "PROJECTS.md"
	ProjectFile  = "PROJECT.md"
	ReadmeFile   = "README.md"
)

// filePerm matches internal/config: state under the Apex root is single-user
// and nothing in it is group- or world-readable.
const filePerm os.FileMode = 0o600

// ContextDir returns the directory holding the identity documents and the
// registry, given the Apex state root (DESIGN.md §5).
func ContextDir(root string) string { return filepath.Join(root, "context") }

// ProfilePath returns <root>/context/PROFILE.md.
func ProfilePath(root string) string { return filepath.Join(ContextDir(root), ProfileFile) }

// SkillsPath returns <root>/context/SKILLS.md.
func SkillsPath(root string) string { return filepath.Join(ContextDir(root), SkillsFile) }

// RegistryPath returns <root>/context/PROJECTS.md.
func RegistryPath(root string) string { return filepath.Join(ContextDir(root), ProjectsFile) }

// ProjectDocPath returns <dir>/PROJECT.md for a project directory.
func ProjectDocPath(dir string) string { return filepath.Join(dir, ProjectFile) }

// ReadmePath returns <dir>/README.md for a project directory.
func ReadmePath(dir string) string { return filepath.Join(dir, ReadmeFile) }

// MalformedError reports a context file Apex could not parse. It is
// user-fixable: the message names the file, and the line when there is one.
type MalformedError struct {
	Path string
	Line int // 1-based; 0 when the problem is not line-specific
	Msg  string
	Err  error
}

func (e *MalformedError) Error() string {
	var b strings.Builder
	b.WriteString(e.Path)
	if e.Line > 0 {
		fmt.Fprintf(&b, ":%d", e.Line)
	}
	b.WriteString(": ")
	b.WriteString(e.Msg)
	if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	return b.String()
}

// UserFixable marks a malformed file as something the user edits, not a bug in
// Apex.
func (e *MalformedError) UserFixable() bool { return true }

func (e *MalformedError) Unwrap() error { return e.Err }

// Document is a markdown file read whole. Present is reported separately from
// Body so a caller can distinguish "the user has not written one yet" from "the
// user wrote one and left it empty" — a distinction digest assembly needs.
type Document struct {
	Path    string
	Body    string
	Present bool
}

// Empty reports whether the document is absent or contains only whitespace.
func (d Document) Empty() bool { return !d.Present || strings.TrimSpace(d.Body) == "" }

// LoadDocument reads a markdown file. A file that does not exist yields a
// Document with Present false and no error.
func LoadDocument(ctx context.Context, path string) (Document, error) {
	if err := ctx.Err(); err != nil {
		return Document{}, err
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return Document{Path: path}, nil
	case err != nil:
		return Document{}, fmt.Errorf("read %s: %w", path, err)
	}
	return Document{Path: path, Body: string(data), Present: true}, nil
}

// Identity is the hand-written context loaded into every advisor system prompt
// (DESIGN.md §6). Apex never writes to either document in v1.
type Identity struct {
	Profile Document
	Skills  Document
}

// Empty reports whether neither document carries any content.
func (i Identity) Empty() bool { return i.Profile.Empty() && i.Skills.Empty() }

// LoadIdentity reads PROFILE.md and SKILLS.md from the Apex context directory.
// Neither being present is a valid state, not an error.
func LoadIdentity(ctx context.Context, root string) (Identity, error) {
	profile, err := LoadDocument(ctx, ProfilePath(root))
	if err != nil {
		return Identity{}, err
	}
	skills, err := LoadDocument(ctx, SkillsPath(root))
	if err != nil {
		return Identity{}, err
	}
	return Identity{Profile: profile, Skills: skills}, nil
}

// writeAtomic writes data to path by way of a temporary file in the same
// directory followed by a rename, so an interrupted write can never leave a
// truncated file behind. The registry is hand-maintained and irreplaceable;
// a partial write of it is the worst outcome this package can produce.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	perm := filePerm
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}

	// The temporary file must share a directory with the target: rename is
	// only atomic within one filesystem.
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp file beside %s: %w", path, err)
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(perm); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmpName, path, err)
	}
	renamed = true

	// Fsync the directory so the rename itself is durable, not just the
	// bytes it points at.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// splitLines splits data into physical lines, each keeping its own terminator
// ("\n", "\r\n", or none on a final line that lacks one). Concatenating the
// result reproduces data exactly; that property is what makes the PROJECTS.md
// round trip byte-identical, including mixed line endings.
func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	var lines []string
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			lines = append(lines, string(data[start:i+1]))
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, string(data[start:]))
	}
	return lines
}

// joinLines is the inverse of splitLines.
func joinLines(lines []string) []byte {
	var buf bytes.Buffer
	for _, l := range lines {
		buf.WriteString(l)
	}
	return buf.Bytes()
}

// lineText strips the terminator from a physical line.
func lineText(line string) string {
	line = strings.TrimSuffix(line, "\n")
	return strings.TrimSuffix(line, "\r")
}

// lineEnding returns the terminator of a physical line, or "" when the line is
// the last in a file that does not end with a newline.
func lineEnding(line string) string {
	if strings.HasSuffix(line, "\r\n") {
		return "\r\n"
	}
	if strings.HasSuffix(line, "\n") {
		return "\n"
	}
	return ""
}
