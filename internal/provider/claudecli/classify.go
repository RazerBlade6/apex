package claudecli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/RazerBlade6/apex/internal/provider"
)

// startError classifies a process that never started.
func startError(op string, err error) *provider.Error {
	var notFound *exec.Error
	if errors.As(err, &notFound) {
		return notInstalled(op, err)
	}
	return &provider.Error{
		Provider: Name, Op: op, Kind: provider.KindUnknown,
		Message: provider.Truncate(err.Error()), Err: err,
	}
}

// sessionLimitPhrases are how a subscription window announces it is spent.
//
// These are matched against the CLI's own error text, not against anything
// Apex composes. They are the weaker of the two signals: the structured
// rate_limit_event is checked first and is what normally decides. They exist
// because the one real invocation this package was built against returned a
// window with 26% utilisation, so the exhausted form of that event was never
// observed, and a provider that could only recognise a session limit through
// an unobserved field would recognise it never.
//
// Every phrase is specific to a subscription window. "rate limit" is
// deliberately absent: it is the API failure this kind exists to be distinct
// from, and it is classified from api_error_status instead.
var sessionLimitPhrases = []string{
	"usage limit reached",
	"session limit",
	"5-hour limit",
	"five-hour limit",
	"weekly limit",
	"limit reached · resets",
}

// loggedOutPhrases are how the CLI says it has no credential.
//
// Unverified for the same reason: logging the user out to observe it would
// have cost them their session. They are matched only after the structured
// signals, and a miss degrades to KindUnknown with the CLI's own message
// quoted, which is still actionable.
var loggedOutPhrases = []string{
	"not logged in",
	"please log in",
	"claude auth login",
	"/login",
	"invalid api key",
	"authentication_error",
	"unauthorized",
	"oauth token has expired",
	"credentials have expired",
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// classify decides whether a finished run failed, and if so how.
//
// The order is the whole design. A cancelled context is not a failure of the
// provider; a session limit is not a rate limit; a logged-out CLI is not a
// missing binary. Each is separated before the fallback that would blur it.
func classify(
	ctx context.Context,
	op string,
	out *outcome,
	result *line,
	limit *rateLimitInfo,
	waitErr, scanErr error,
	stderr *tailBuffer,
) *provider.Error {
	// 1. The caller left. exec.CommandContext killed the process group, so
	//    "signal: killed" in waitErr says nothing about the provider.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return &provider.Error{
			Provider: Name, Op: op, Kind: provider.KindCancelled,
			Message: "the run was cancelled",
			Err:     ctxErr,
		}
	}

	failed := waitErr != nil || scanErr != nil || (result != nil && result.IsError)
	if !failed {
		return nil
	}

	// The CLI's own words, never Apex's argv or environment. Both sources are
	// bounded before they reach an error string.
	detail := strings.TrimSpace(out.result)
	if tail := strings.TrimSpace(stderr.String()); tail != "" {
		if detail != "" {
			detail += "; "
		}
		detail += tail
	}
	lower := strings.ToLower(detail)

	// 2. A subscription window that is spent. Read only on a failed run:
	//    the rate_limit_event's non-exhausted values are not fully known, so
	//    a successful stream is never reported as limited.
	if !limit.permissive() || containsAny(lower, sessionLimitPhrases) {
		msg := "the Claude subscription's usage window is exhausted"
		if w := limit.window(); w != "" {
			msg = fmt.Sprintf("the Claude subscription's %s usage window is exhausted", w)
		}
		if detail != "" {
			msg += ": " + detail
		}
		return &provider.Error{
			Provider: Name, Op: op, Kind: provider.KindSessionLimit,
			Message:  provider.Truncate(msg),
			ResetsAt: limit.resets(),
			Err:      waitErr,
		}
	}

	// 3. An HTTP status the CLI surfaced. This is where a genuine API rate
	//    limit lands, which is why it is kept downstream of the session
	//    check rather than merged with it.
	if result != nil && result.APIErrorStatus != nil && *result.APIErrorStatus != 0 {
		status := *result.APIErrorStatus
		return &provider.Error{
			Provider: Name, Op: op, Kind: provider.StatusKind(status),
			StatusCode: status,
			Message:    provider.Truncate(detail),
			Err:        waitErr,
		}
	}

	// 4. No credential. DESIGN.md §13: the fix is `claude auth login`, never
	//    an API key in the keychain.
	if containsAny(lower, loggedOutPhrases) {
		msg := fmt.Sprintf("the %s CLI is not logged in", Binary)
		if detail != "" {
			msg += ": " + detail
		}
		return &provider.Error{
			Provider: Name, Op: op, Kind: provider.KindAuth,
			Message: provider.Truncate(msg), Err: waitErr,
		}
	}

	// 5. Anything else, with the exit status and the CLI's message intact.
	msg := detail
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		if msg == "" {
			msg = "no output"
		}
		msg = fmt.Sprintf("%s exited %d: %s", Binary, exitErr.ExitCode(), msg)
	} else if scanErr != nil {
		if msg == "" {
			msg = scanErr.Error()
		}
		msg = "reading the event stream failed: " + msg
	} else if msg == "" {
		msg = fmt.Sprintf("%s reported a failure without saying what", Binary)
	}

	err := waitErr
	if err == nil {
		err = scanErr
	}
	return &provider.Error{
		Provider: Name, Op: op, Kind: provider.KindUnknown,
		Message: provider.Truncate(msg), Err: err,
	}
}
