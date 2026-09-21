package project

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Git state is read by shelling out to git rather than by linking a Go
// implementation. git is already a hard dependency (DESIGN.md §15, and
// `apex doctor` checks for it), it is the only thing guaranteed to agree with
// what the user sees in the same directory, and the four facts Apex wants are
// four one-line commands.
//
// A directory that is not a repository is a perfectly good project. It is
// reported, not rejected.

// logLimit is the number of commits assembled as a digest source
// (DESIGN.md §6: `git log --oneline -20`).
const logLimit = 20

// gitTimeout bounds each git invocation. A repository on a stalled network
// mount must not hang a sync over the rest of the portfolio.
const gitTimeout = 20 * time.Second

// GitState is what Apex knows about a project's repository.
type GitState struct {
	// IsRepo is false for a directory that is not inside a git work tree.
	IsRepo bool
	// Root is the work tree root, which may be above Path in a monorepo.
	Root string
	// Head is the full HEAD SHA, or "" in a repository with no commits yet.
	Head string
	// Branch is the current branch, or "HEAD" when detached.
	Branch string
	// Dirty reports uncommitted changes, including untracked files.
	Dirty bool
	// Status is `git status --short`, one entry per line.
	Status []string
	// StatusRaw is that command's output verbatim. It, not the Dirty
	// boolean, is what the source hash covers (DESIGN.md §6): a boolean
	// stops moving once a tree goes dirty, which freezes the cached digest
	// for exactly the projects being actively worked on.
	StatusRaw string
	// Log is `git log --oneline -20`, newest first.
	Log []string
	// Note explains why the state is absent or partial, for the sync report.
	// It is empty when everything was read cleanly.
	Note string
}

// ShortHead returns the abbreviated HEAD SHA for display.
func (g GitState) ShortHead() string {
	if len(g.Head) < 7 {
		return g.Head
	}
	return g.Head[:7]
}

// Describe renders the state as one cell of the sync table.
func (g GitState) Describe() string {
	if !g.IsRepo {
		if g.Note != "" {
			return g.Note
		}
		return "not a git repository"
	}
	head := g.ShortHead()
	if head == "" {
		head = "no commits"
	}
	parts := []string{head}
	if g.Branch != "" && g.Branch != "HEAD" {
		parts = append(parts, g.Branch)
	} else if g.Branch == "HEAD" {
		parts = append(parts, "detached")
	}
	if g.Dirty {
		parts = append(parts, fmt.Sprintf("dirty:%d", len(g.Status)))
	} else {
		parts = append(parts, "clean")
	}
	return strings.Join(parts, " ")
}

// InspectGit reads the repository state of a directory.
//
// It returns an error only when the context is done. Everything else — git
// missing from PATH, a directory that is not a repository, a repository with no
// commits — is a state to report, so one odd project degrades instead of
// aborting the sync around it.
func InspectGit(ctx context.Context, dir string) (GitState, error) {
	if err := ctx.Err(); err != nil {
		return GitState{}, err
	}

	if _, err := exec.LookPath("git"); err != nil {
		return GitState{Note: "git is not on PATH"}, nil
	}

	root, err := gitOutput(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return GitState{}, ctxErr
		}
		// The overwhelmingly common cause is "not a git repository", which
		// git reports on stderr with a non-zero exit. That is an ordinary
		// state, so it gets a short note rather than git's fatal message.
		note := gitNote(err)
		if strings.Contains(note, "not a git repository") {
			note = "not a git repository"
		}
		return GitState{Note: note}, nil
	}

	g := GitState{IsRepo: true, Root: strings.TrimSpace(root)}

	// An unborn HEAD (a repository with no commits) fails rev-parse. That is
	// a real state, not a failure: leave Head empty and carry on.
	if head, err := gitOutput(ctx, dir, "rev-parse", "HEAD"); err == nil {
		g.Head = strings.TrimSpace(head)
	} else if ctxErr := ctx.Err(); ctxErr != nil {
		return GitState{}, ctxErr
	}

	if branch, err := gitOutput(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		g.Branch = strings.TrimSpace(branch)
	}

	// --no-optional-locks keeps `status` from refreshing the index, so
	// reading a project under a shared lock cannot write to its working tree.
	if status, err := gitOutput(ctx, dir, "--no-optional-locks", "status", "--short"); err == nil {
		g.StatusRaw = status
		g.Status = splitNonEmptyLines(status)
		g.Dirty = len(g.Status) > 0
	} else if ctxErr := ctx.Err(); ctxErr != nil {
		return GitState{}, ctxErr
	} else {
		g.Note = "could not read status: " + gitNote(err)
	}

	if g.Head != "" {
		if log, err := gitOutput(ctx, dir, "log", "--oneline", fmt.Sprintf("-%d", logLimit)); err == nil {
			g.Log = splitNonEmptyLines(log)
		} else if ctxErr := ctx.Err(); ctxErr != nil {
			return GitState{}, ctxErr
		}
	}

	return g, nil
}

// gitOutput runs one git command in dir and returns its standard output.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// A pager or a localised message would turn parseable output into
	// something that only happens to work on this machine.
	cmd.Env = append(cmd.Environ(), "GIT_PAGER=cat", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", &gitError{args: args, dir: dir, stderr: stderr.String(), err: err}
	}
	return stdout.String(), nil
}

// gitError carries git's own message, which says far more than "exit status 128".
type gitError struct {
	args   []string
	dir    string
	stderr string
	err    error
}

func (e *gitError) Error() string {
	msg := fmt.Sprintf("git %s in %s: %v", strings.Join(e.args, " "), e.dir, e.err)
	if s := strings.TrimSpace(e.stderr); s != "" {
		msg += ": " + firstLine(s)
	}
	return msg
}

func (e *gitError) Unwrap() error { return e.err }

// gitNote reduces a git failure to the one line worth putting in a table cell.
func gitNote(err error) string {
	var ge *gitError
	if errors.As(err, &ge) {
		if s := strings.TrimSpace(ge.stderr); s != "" {
			return firstLine(s)
		}
	}
	return firstLine(err.Error())
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}

func splitNonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}
