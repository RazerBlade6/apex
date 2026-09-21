package claudecli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/RazerBlade6/apex/internal/provider"
)

// Binary is the executable looked up on PATH.
const Binary = "claude"

// extraDirs are searched when the binary is not on PATH.
//
// The claude installer puts its launcher in ~/.local/bin, which is on an
// interactive shell's PATH and frequently is not on a cron job's. `apex
// doctor` already searched here for the executor; the provider has to agree
// with it, so the lookup lives in one place and both call it.
func extraDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".local", "bin")}
}

// LookPath resolves the claude binary: PATH first, then the install
// directories a login shell would have added.
//
// A missing binary comes back as a *provider.Error of KindUnavailable, so the
// caller does not have to know that "not installed" is a distinct state from
// "not logged in" — the classification already says so.
func LookPath() (string, error) {
	if path, err := exec.LookPath(Binary); err == nil {
		return path, nil
	}
	for _, dir := range extraDirs() {
		candidate := filepath.Join(dir, Binary)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", notInstalled("lookup", nil)
}

// resolveBinary honours an explicit path and otherwise looks one up.
func resolveBinary(explicit string) (string, error) {
	explicit = strings.TrimSpace(explicit)
	if explicit == "" {
		return LookPath()
	}
	info, err := os.Stat(explicit)
	if err != nil {
		return "", notInstalled("lookup", err)
	}
	if info.IsDir() {
		return "", notInstalled("lookup", fmt.Errorf("%s is a directory", explicit))
	}
	return explicit, nil
}

// notInstalled builds the error for a claude binary that is not on this
// machine. The path is named; nothing else about the environment is, because
// an error string is a place secrets leak.
func notInstalled(op string, cause error) *provider.Error {
	msg := fmt.Sprintf("the %s CLI was not found on PATH", Binary)
	if dirs := extraDirs(); len(dirs) > 0 {
		msg += " or in " + strings.Join(dirs, ", ")
	}
	if cause != nil {
		msg += ": " + cause.Error()
	}
	return &provider.Error{
		Provider: Name,
		Op:       op,
		Kind:     provider.KindUnavailable,
		Message:  provider.Truncate(msg),
		Err:      cause,
	}
}

// AuthStatus is what `claude auth status --json` reports.
//
// Only the fields Apex acts on are decoded. The OAuth credential itself is
// not among them and is not reachable from here: `claude auth status` never
// prints it, Apex never asks for it, and this struct has nowhere to put it
// (DESIGN.md §13).
type AuthStatus struct {
	LoggedIn bool `json:"loggedIn"`
	// AuthMethod is "claude.ai" for a subscription login. An API-key login
	// reports something else, which matters: a claude-cli route served by an
	// API key is not subscription-backed at all, and the user should be told
	// rather than left to discover it on a bill.
	AuthMethod string `json:"authMethod"`
	// APIProvider is "firstParty", or a third party such as Bedrock.
	APIProvider string `json:"apiProvider"`
	// SubscriptionType is "pro", "max", or empty.
	SubscriptionType string `json:"subscriptionType"`
	// Email and OrgName identify the account. They are the user's own and are
	// not secrets, but nothing here prints them unless a caller chooses to.
	Email   string `json:"email"`
	OrgName string `json:"orgName"`
}

// Subscription reports whether this login is the Claude Pro or Max
// subscription this provider exists to use, rather than an API key that
// happens to be configured in the CLI.
func (s AuthStatus) Subscription() bool {
	return s.LoggedIn && s.AuthMethod == "claude.ai"
}

// Describe renders the account for `apex doctor`, without the org id.
func (s AuthStatus) Describe() string {
	if !s.LoggedIn {
		return "not logged in"
	}
	var b strings.Builder
	b.WriteString("logged in")
	if s.Email != "" {
		fmt.Fprintf(&b, " as %s", s.Email)
	}
	switch {
	case s.SubscriptionType != "":
		fmt.Fprintf(&b, " (%s subscription", s.SubscriptionType)
		if s.AuthMethod != "" {
			fmt.Fprintf(&b, " via %s", s.AuthMethod)
		}
		b.WriteString(")")
	case s.AuthMethod != "":
		fmt.Fprintf(&b, " (via %s)", s.AuthMethod)
	}
	return b.String()
}

// authTimeout bounds the status call. It reads local state and answers in
// milliseconds; anything slower is a finding, and doctor must not hang.
const authTimeout = 15 * time.Second

// Auth runs `claude auth status --json` and decodes it.
//
// bin may be empty, in which case the binary is looked up. Three outcomes are
// kept apart, because they have three different fixes:
//
//   - not installed       -> *provider.Error, KindUnavailable
//   - installed, logged out -> AuthStatus{LoggedIn: false}, nil error
//   - logged in           -> AuthStatus{LoggedIn: true}, nil error
//
// A logged-out CLI is not an error here: it is a fact the caller reports. It
// becomes a KindAuth failure only when something actually tries to infer.
func Auth(ctx context.Context, bin string) (AuthStatus, error) {
	path, err := resolveBinary(bin)
	if err != nil {
		return AuthStatus{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, "auth", "status", "--json")
	cmd.Stdin = nil
	var stderr tailBuffer
	cmd.Stderr = &stderr
	configureProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = killGrace

	stdout, runErr := cmd.Output()

	// The exit status of a logged-out CLI is not documented, so the JSON is
	// decoded first and the exit status consulted only if there is no JSON to
	// read. That way "logged out" is reported from what the CLI said rather
	// than from a status code guessed at.
	var status AuthStatus
	if jsonErr := json.Unmarshal(stdout, &status); jsonErr == nil {
		return status, nil
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return AuthStatus{}, &provider.Error{
			Provider: Name, Op: "auth", Kind: provider.KindCancelled,
			Message: fmt.Sprintf("%s auth status did not answer within %s", Binary, authTimeout),
			Err:     ctxErr,
		}
	}

	var notFound *exec.Error
	if errors.As(runErr, &notFound) {
		return AuthStatus{}, notInstalled("auth", runErr)
	}

	detail := strings.TrimSpace(stderr.String())
	if detail == "" {
		detail = strings.TrimSpace(string(stdout))
	}
	if detail == "" && runErr != nil {
		detail = runErr.Error()
	}
	return AuthStatus{}, &provider.Error{
		Provider: Name, Op: "auth", Kind: provider.KindUnknown,
		Message: provider.Truncate(fmt.Sprintf("%s auth status returned no usable JSON: %s", Binary, detail)),
		Err:     runErr,
	}
}
