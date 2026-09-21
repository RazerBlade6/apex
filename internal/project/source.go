package project

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"

	"github.com/RazerBlade6/apex/internal/contextfs"
)

// SourceHashVersion labels the byte layout SourceHash feeds to SHA-256. It is
// part of the hashed stream, so changing the layout changes every hash and
// every cached digest is correctly treated as stale, rather than a new binary
// silently accepting a hash an old one computed differently.
const SourceHashVersion = "apex-source-hash-v2"

// HashInputs is exactly what source_hash covers: every digest source named in
// DESIGN.md §6 except the git log, which HEAD already summarises.
//
// It is a struct so the inputs are named at the call site, but the hashed byte
// layout below is written out explicitly and does NOT follow from this field
// order — two binaries must agree on the stream, not on a struct definition.
type HashInputs struct {
	// ProjectDoc is PROJECT.md verbatim; HasProjectDoc distinguishes an
	// absent file from an empty one.
	ProjectDoc    string
	HasProjectDoc bool
	// Readme is README.md verbatim; HasReadme distinguishes absent from empty.
	Readme    string
	HasReadme bool
	// Head is the full HEAD SHA, empty when unborn or not a repository.
	Head string
	// GitStatus is the raw `git status --short` output, empty for a clean
	// tree or a directory that is not a repository.
	GitStatus string
}

// SourceHash computes the cache key a digest is stored against (DESIGN.md §6).
//
// M4's caching correctness depends on this being reproducible, so the hashed
// bytes are specified exactly. The stream is, with no separators beyond the
// newlines shown:
//
//	apex-source-hash-v2\n
//	project.md:absent\n            when PROJECT.md does not exist
//	project.md:<N>\n<N bytes>\n    otherwise
//	readme.md:absent\n             when README.md does not exist
//	readme.md:<N>\n<N bytes>\n     otherwise
//	head:<sha>\n                   empty when unborn or not a repository
//	status:<64 hex chars>\n        SHA-256 of `git status --short`
//
// and the result is the lowercase hex SHA-256 of it. N is the byte length
// after CRLF -> LF normalisation, which is also what is hashed.
//
// Four decisions make that stable and correct across runs and machines:
//
//   - The project's path is not hashed. The same commit checked out at two
//     paths, on two machines, is the same project state.
//   - Bodies are length-prefixed, so no content can imitate the field that
//     follows it and two different inputs cannot hash alike.
//   - CRLF is normalised to LF, so a checkout that converted line endings does
//     not invalidate every cached digest.
//   - The working tree is represented by a digest of `git status --short`,
//     never by a dirty boolean. A boolean stops moving after the first
//     uncommitted change, so every later edit to an already-dirty tree would
//     be invisible and the cached digest would freeze for precisely the
//     projects under active work. Hashing README.md serves the same end for a
//     project that is not a git repository at all, where nothing else moves.
//
// The git log is deliberately not hashed: HEAD already identifies it exactly.
func SourceHash(in HashInputs) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n", SourceHashVersion)
	hashBody(h, "project.md", in.ProjectDoc, in.HasProjectDoc)
	hashBody(h, "readme.md", in.Readme, in.HasReadme)
	fmt.Fprintf(h, "head:%s\n", in.Head)
	fmt.Fprintf(h, "status:%s\n", StatusDigest(in.GitStatus))
	return hex.EncodeToString(h.Sum(nil))
}

// hashBody writes one length-prefixed, absence-aware file field.
func hashBody(h hash.Hash, label, body string, present bool) {
	if !present {
		fmt.Fprintf(h, "%s:absent\n", label)
		return
	}
	normalised := normaliseNewlines(body)
	fmt.Fprintf(h, "%s:%d\n", label, len(normalised))
	h.Write([]byte(normalised))
	h.Write([]byte("\n"))
}

// StatusDigest is the SHA-256 of `git status --short` output that stands in for
// the working tree's state. An empty status — a clean tree, or a directory that
// is not a repository — hashes to the SHA-256 of the empty string, which is
// stable and is what "nothing uncommitted" means in both cases.
func StatusDigest(status string) string {
	sum := sha256.Sum256([]byte(normaliseNewlines(status)))
	return hex.EncodeToString(sum[:])
}

func normaliseNewlines(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

// DigestSource is everything a digest is generated from, assembled but not yet
// summarised. M4 hands this to a provider; M2 only fills it in.
//
// The field order mirrors the priority order in DESIGN.md §6: PROJECT.md is
// authoritative because it is the user's own words, README.md is next, and the
// git log and status describe what has actually been happening.
type DigestSource struct {
	Slug string
	Name string
	Path string

	// Doc is the parsed PROJECT.md. Doc.Present is false when the project
	// has none, which is a valid state and not an error.
	Doc *contextfs.ProjectDoc
	// Readme is README.md, if the project has one.
	Readme contextfs.Document
	// Git is the repository state, which may report that there is none.
	Git GitState

	// SourceHash is the cache key these sources hash to.
	SourceHash string
}

// HasProjectDoc reports whether the project describes itself.
func (s *DigestSource) HasProjectDoc() bool { return s.Doc != nil && s.Doc.Present }

// AssembleSource gathers the digest sources for one project and computes its
// source hash. It reads; it never writes to the project directory.
//
// The caller is expected to be holding a shared lock on the project
// (DESIGN.md §10), so these reads cannot observe a working tree halfway
// through a dispatch.
func AssembleSource(ctx context.Context, c Candidate) (*DigestSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	doc, err := contextfs.LoadProjectDoc(ctx, c.Path)
	if err != nil {
		return nil, fmt.Errorf("read %s for %s: %w", contextfs.ProjectFile, c.Name, err)
	}

	readme, err := contextfs.LoadDocument(ctx, contextfs.ReadmePath(c.Path))
	if err != nil {
		return nil, fmt.Errorf("read %s for %s: %w", contextfs.ReadmeFile, c.Name, err)
	}

	git, err := InspectGit(ctx, c.Path)
	if err != nil {
		return nil, fmt.Errorf("inspect git for %s: %w", c.Name, err)
	}

	src := &DigestSource{
		Slug:   c.Slug,
		Name:   c.Name,
		Path:   c.Path,
		Doc:    doc,
		Readme: readme,
		Git:    git,
	}
	src.SourceHash = SourceHash(HashInputs{
		ProjectDoc:    doc.Raw,
		HasProjectDoc: doc.Present,
		Readme:        readme.Body,
		HasReadme:     readme.Present,
		Head:          git.Head,
		GitStatus:     git.StatusRaw,
	})
	return src, nil
}

// summaryLimit is roughly one terminal line. A generated registry summary is a
// glance, not a digest.
const summaryLimit = 160

// Summarize renders the summary line `apex sync` writes under a registry entry,
// deterministically, from the project's own PROJECT.md.
//
// It returns "" when there is nothing to say, and the caller must then leave
// the existing line alone: overwriting a line the user wrote with a placeholder
// saying Apex had nothing would be a strictly worse file.
//
// This is not the digest. A digest is a generated synthesis of several sources
// (DESIGN.md §6) and needs a provider; this is the lead sentence of the
// project's own description plus its declared status and stack.
func Summarize(doc *contextfs.ProjectDoc) string {
	if doc == nil || !doc.Present {
		return ""
	}

	lead := truncateWords(collapse(doc.Lead()), summaryLimit)

	var meta []string
	if status := strings.TrimSpace(doc.Status()); status != "" {
		meta = append(meta, status)
	}
	if stack := doc.Stack(); len(stack) > 0 {
		cleaned := make([]string, 0, len(stack))
		for _, s := range stack {
			if s = strings.TrimSpace(s); s != "" {
				cleaned = append(cleaned, s)
			}
		}
		if len(cleaned) > 0 {
			meta = append(meta, strings.Join(cleaned, ", "))
		}
	}

	switch {
	case lead != "" && len(meta) > 0:
		return lead + " · " + strings.Join(meta, " · ")
	case lead != "":
		return lead
	case len(meta) > 0:
		return strings.Join(meta, " · ")
	default:
		return ""
	}
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// truncateWords shortens a string to at most limit bytes, on a word boundary,
// marking that it was cut.
func truncateWords(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := s[:limit]
	if i := strings.LastIndexByte(cut, ' '); i > 0 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " .,;:") + "…"
}
