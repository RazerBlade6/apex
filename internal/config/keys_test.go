package config

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestResolveKey(t *testing.T) {
	const (
		keychainSecret = "sk-from-keychain"
		envSecret      = "sk-from-env"
	)

	tests := []struct {
		name       string
		provider   string
		inKeychain string
		inEnv      string
		wantSource KeySource
		wantOrigin string
		wantValue  string
		wantErr    bool
	}{
		{
			name:       "keychain wins",
			provider:   "anthropic",
			inKeychain: keychainSecret,
			inEnv:      envSecret,
			wantSource: SourceKeychain,
			wantOrigin: "apex:anthropic",
			wantValue:  keychainSecret,
		},
		{
			name:       "env fallback",
			provider:   "anthropic",
			inEnv:      envSecret,
			wantSource: SourceEnv,
			wantOrigin: "ANTHROPIC_API_KEY",
			wantValue:  envSecret,
		},
		{
			name:       "openai env fallback",
			provider:   "openai",
			inEnv:      envSecret,
			wantSource: SourceEnv,
			wantOrigin: "OPENAI_API_KEY",
			wantValue:  envSecret,
		},
		{
			name:       "whitespace is trimmed",
			provider:   "openai",
			inKeychain: "  " + keychainSecret + "\n",
			wantSource: SourceKeychain,
			wantOrigin: "apex:openai",
			wantValue:  keychainSecret,
		},
		{
			name:     "neither source",
			provider: "anthropic",
			wantErr:  true,
		},
		{
			name:       "blank env is not a key",
			provider:   "anthropic",
			inEnv:      "   ",
			wantErr:    true,
			wantSource: "",
		},
		{
			name:     "unknown provider",
			provider: "mistral",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A fresh mock per subtest keeps the real login keychain out of
			// the tests entirely.
			keyring.MockInit()

			service, envVar, err := CredentialLocation(tt.provider)
			if err == nil {
				if tt.inKeychain != "" {
					if err := keyring.Set(service, KeychainUser, tt.inKeychain); err != nil {
						t.Fatalf("seed keychain: %v", err)
					}
				}
				t.Setenv(envVar, tt.inEnv)
			}

			key, err := ResolveKey(context.Background(), tt.provider)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ResolveKey() = %v, want an error", key)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveKey: %v", err)
			}
			if key.Source != tt.wantSource {
				t.Errorf("Source = %q, want %q", key.Source, tt.wantSource)
			}
			if key.Origin != tt.wantOrigin {
				t.Errorf("Origin = %q, want %q", key.Origin, tt.wantOrigin)
			}
			if key.Value() != tt.wantValue {
				t.Errorf("Value() did not match the seeded secret")
			}
		})
	}
}

func TestMissingKeyErrorNamesBothSources(t *testing.T) {
	keyring.MockInit()
	t.Setenv("ANTHROPIC_API_KEY", "")

	_, err := ResolveKey(context.Background(), "anthropic")
	var missing *MissingKeyError
	if !errors.As(err, &missing) {
		t.Fatalf("ResolveKey error = %v, want *MissingKeyError", err)
	}
	if !missing.UserFixable() {
		t.Error("MissingKeyError should be user-fixable")
	}
	for _, want := range []string{"apex:anthropic", "ANTHROPIC_API_KEY", "security add-generic-password"} {
		if !strings.Contains(missing.Error(), want) {
			t.Errorf("error message does not mention %q:\n%s", want, missing.Error())
		}
	}
}

func TestKeyNeverPrintsItsValue(t *testing.T) {
	keyring.MockInit()
	const secret = "sk-super-secret-value"
	if err := keyring.Set("apex:anthropic", KeychainUser, secret); err != nil {
		t.Fatal(err)
	}

	key, err := ResolveKey(context.Background(), "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	for _, rendered := range []string{
		key.String(),
		fmt.Sprint(key),
		fmt.Sprintf("%v", key),
		fmt.Sprintf("%s", key),
		fmt.Sprintf("%#v", key),
		fmt.Sprintf("%+v", key),
	} {
		if strings.Contains(rendered, secret) {
			t.Errorf("formatted Key leaked the secret: %s", rendered)
		}
	}
}

// TestClaudeCLIHasNoAPIKeyToResolve is the §13 guarantee for M3.5: a route
// configured for the subscription-backed provider must never be reported as
// missing an API key, because that message sends the user to the keychain to
// add a key nothing would ever read.
func TestClaudeCLIHasNoAPIKeyToResolve(t *testing.T) {
	keyring.MockInit()

	kind, err := CredentialKindOf("claude-cli")
	if err != nil {
		t.Fatalf("CredentialKindOf: %v", err)
	}
	if kind != CredentialSubscription {
		t.Errorf("kind = %q, want %q", kind, CredentialSubscription)
	}
	if UsesAPIKey("claude-cli") {
		t.Error("UsesAPIKey(claude-cli) = true")
	}

	_, err = ResolveKey(context.Background(), "claude-cli")
	var keyless *KeylessProviderError
	if !errors.As(err, &keyless) {
		t.Fatalf("err = %T (%v), want *KeylessProviderError", err, err)
	}
	var missing *MissingKeyError
	if errors.As(err, &missing) {
		t.Fatal("a claude-cli route produced a MissingKeyError; §13 forbids exactly this")
	}
	if strings.Contains(strings.ToLower(err.Error()), "keychain") {
		t.Errorf("err = %q, want no advice about the keychain", err)
	}

	if _, _, err := CredentialLocation("claude-cli"); !errors.As(err, &keyless) {
		t.Errorf("CredentialLocation error = %T, want *KeylessProviderError", err)
	}
}

// TestKeyProvidersExcludesSubscriptionProviders keeps the tables in `apex
// doctor` and `apex config` free of a key column for a provider with no key.
func TestKeyProvidersExcludesSubscriptionProviders(t *testing.T) {
	got := KeyProviders()
	want := []string{"anthropic", "openai"}
	if len(got) != len(want) {
		t.Fatalf("KeyProviders() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("KeyProviders() = %v, want %v", got, want)
		}
	}
}

// TestKeyBasedProvidersAreUnchanged is the M1-M3 regression guard: the
// generalisation to credentials must not alter how a key-based route
// resolves.
func TestKeyBasedProvidersAreUnchanged(t *testing.T) {
	for _, name := range []string{"anthropic", "openai"} {
		if !UsesAPIKey(name) {
			t.Errorf("UsesAPIKey(%q) = false", name)
		}
		service, envVar, err := CredentialLocation(name)
		if err != nil {
			t.Fatalf("CredentialLocation(%q): %v", name, err)
		}
		if service == "" || envVar == "" {
			t.Errorf("CredentialLocation(%q) = %q, %q; want both populated", name, service, envVar)
		}
	}
}
