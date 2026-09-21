package claudecli

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/RazerBlade6/apex/internal/provider"
)

// The JSON below is what `claude auth status --json` printed on 2026-09-21,
// with the org id removed. The logged-out and API-key variants are
// constructed from the same fields, because observing them for real would
// have meant logging the user out of the subscription this milestone exists
// to use.
const authLoggedIn = `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty",` +
	`"analyticsDisabled":false,"email":"user@example.com",` +
	`"orgName":"user@example.com's Organization","subscriptionType":"pro"}`

func TestAuthLoggedIn(t *testing.T) {
	// The fixture contains an apostrophe, so it is written with a heredoc
	// rather than through emit's single-quoted printf.
	f := newFake(t, "cat <<'JSON'\n"+authLoggedIn+"\nJSON\n")

	status, err := Auth(context.Background(), f.bin)
	if err != nil {
		t.Fatalf("Auth: %v", err)
	}
	if !status.LoggedIn {
		t.Fatal("LoggedIn = false for a logged-in account")
	}
	if !status.Subscription() {
		t.Error("Subscription() = false for a claude.ai login")
	}
	if got := status.Describe(); !strings.Contains(got, "user@example.com") || !strings.Contains(got, "pro") {
		t.Errorf("Describe() = %q, want the account and the subscription type", got)
	}

	args := f.args(t)
	for _, want := range []string{"auth", "status", "--json"} {
		if !slices.Contains(args, want) {
			t.Errorf("argv = %q, want %q", args, want)
		}
	}
}

func TestAuthLoggedOutIsAFactNotAnError(t *testing.T) {
	// Exit status is deliberately non-zero here: the CLI's exit code for a
	// logged-out account is not documented, and Auth must read the JSON
	// rather than the status.
	f := newFake(t, `printf '%s\n' '{"loggedIn":false}'`+"\nexit 1\n")

	status, err := Auth(context.Background(), f.bin)
	if err != nil {
		t.Fatalf("Auth reported an error for a logged-out CLI: %v", err)
	}
	if status.LoggedIn {
		t.Error("LoggedIn = true for a logged-out CLI")
	}
	if status.Subscription() {
		t.Error("Subscription() = true for a logged-out CLI")
	}
	if got := status.Describe(); got != "not logged in" {
		t.Errorf("Describe() = %q, want %q", got, "not logged in")
	}
}

// TestAuthAPIKeyLoginIsNotASubscription covers the case a claude-cli route
// most wants to know about: the CLI is logged in, and the login bills an API
// account rather than the subscription the route was chosen for.
func TestAuthAPIKeyLoginIsNotASubscription(t *testing.T) {
	f := newFake(t, `printf '%s\n' '{"loggedIn":true,"authMethod":"apiKey","apiProvider":"firstParty"}'`+"\n")

	status, err := Auth(context.Background(), f.bin)
	if err != nil {
		t.Fatalf("Auth: %v", err)
	}
	if !status.LoggedIn {
		t.Fatal("LoggedIn = false")
	}
	if status.Subscription() {
		t.Error("Subscription() = true for an API-key login")
	}
}

func TestAuthNotInstalled(t *testing.T) {
	_, err := Auth(context.Background(), filepath.Join(t.TempDir(), "absent"))
	var perr *provider.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err = %T (%v), want *provider.Error", err, err)
	}
	if perr.Kind != provider.KindUnavailable {
		t.Errorf("kind = %v, want %v", perr.Kind, provider.KindUnavailable)
	}
}

func TestAuthUnreadableOutput(t *testing.T) {
	f := newFake(t, "printf 'command not found\\n' >&2\nexit 127\n")

	_, err := Auth(context.Background(), f.bin)
	var perr *provider.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err = %T (%v), want *provider.Error", err, err)
	}
	if perr.Kind != provider.KindUnknown {
		t.Errorf("kind = %v, want %v", perr.Kind, provider.KindUnknown)
	}
	if !strings.Contains(perr.Message, "command not found") {
		t.Errorf("message = %q, want the CLI's own output", perr.Message)
	}
}
