// Package claudecmd is the one place Apex knows how to find and supervise the
// `claude` binary.
//
// Two callers run that binary for opposite reasons. internal/provider/claudecli
// runs it as a pure inference transport with every tool disabled (DESIGN.md §8);
// internal/executor/claudecode runs it as a coding agent with tools enabled,
// which is its entire job (DESIGN.md §9). Their command lines have almost
// nothing in common and must not be shared. What they do share is everything
// below: where the binary lives, how a cancelled run is killed without leaving
// orphans, and how to read the stream-json it emits.
//
// DESIGN.md §18 recorded the first of those as an open item — "doctor resolves
// the claude binary twice, two different ways ... M5 should collapse the
// executor onto the provider's lookup so they cannot drift". This package is
// that collapse, widened to the other two because the same argument applies:
// two copies of a wire format agree until the day one of them is updated.
package claudecmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Binary is the executable name looked up on PATH.
const Binary = "claude"

// ExtraDirs are searched when the binary is not on PATH.
//
// The claude installer puts its launcher in ~/.local/bin, which is on an
// interactive shell's PATH and frequently is not on a cron job's. Apex is
// meant to be cron-able (DESIGN.md §11), so the lookup cannot depend on a
// login shell having run.
func ExtraDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".local", "bin")}
}

// NotFoundError reports that the claude binary is not on this machine.
//
// It is deliberately a plain typed error rather than a *provider.Error: this
// package is below both callers, and each wraps it in the vocabulary of its
// own layer — KindUnavailable for the provider, a dispatch refusal for the
// executor. The path is named; nothing else about the environment is, because
// an error string is a place secrets leak.
type NotFoundError struct {
	// Searched is where Apex looked, beyond PATH.
	Searched []string
	// Err is the underlying cause, when there was one. A binary that is
	// simply absent has none.
	Err error
}

func (e *NotFoundError) Error() string {
	msg := fmt.Sprintf("the %s CLI was not found on PATH", Binary)
	if len(e.Searched) > 0 {
		msg += " or in " + strings.Join(e.Searched, ", ")
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *NotFoundError) Unwrap() error { return e.Err }

// UserFixable marks a missing binary as an environment problem: the fix is to
// install the CLI, not to change Apex.
func (e *NotFoundError) UserFixable() bool { return true }

// LookPath resolves the claude binary: PATH first, then the directories its
// installer uses that a non-login shell would not have added.
func LookPath() (string, error) {
	if path, err := exec.LookPath(Binary); err == nil {
		return path, nil
	}
	dirs := ExtraDirs()
	for _, dir := range dirs {
		candidate := filepath.Join(dir, Binary)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", &NotFoundError{Searched: dirs}
}

// Resolve honours an explicitly configured path and otherwise looks one up.
//
// An explicit path that does not exist is an error rather than a fallback to
// PATH: a user who configured a path meant that path, and silently running a
// different binary would be worse than saying so.
func Resolve(explicit string) (string, error) {
	explicit = strings.TrimSpace(explicit)
	if explicit == "" {
		return LookPath()
	}
	info, err := os.Stat(explicit)
	if err != nil {
		return "", &NotFoundError{Err: err}
	}
	if info.IsDir() {
		return "", &NotFoundError{Err: fmt.Errorf("%s is a directory", explicit)}
	}
	return explicit, nil
}
