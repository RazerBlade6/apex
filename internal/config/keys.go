package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/zalando/go-keyring"
)

// KeySource records where a resolved API key came from. The key value itself is
// never logged, printed, or written to disk (DESIGN.md §13).
type KeySource string

const (
	SourceKeychain KeySource = "keychain"
	SourceEnv      KeySource = "env"
)

// KeychainUser is the account name stored under each keychain service. Apex is
// single-user, so one entry per provider is enough.
const KeychainUser = "default"

// Key describes a resolved credential without exposing it. String and GoString
// are defined so a Key can never be accidentally printed in full.
type Key struct {
	Provider string
	Source   KeySource
	Origin   string // "apex:anthropic" or "ANTHROPIC_API_KEY"

	value string
}

// Value returns the secret. Callers pass it straight to a provider SDK; it must
// not be logged.
func (k Key) Value() string { return k.value }

// String redacts the key.
func (k Key) String() string {
	return fmt.Sprintf("%s key from %s (%s)", k.Provider, k.Source, k.Origin)
}

// GoString redacts the key under %#v as well.
func (k Key) GoString() string { return k.String() }

// MissingKeyError is returned when no credential could be found for a provider.
// It is user-fixable: the message names both places Apex looked.
type MissingKeyError struct {
	Provider string
	Service  string
	EnvVar   string
	// Cause is the keychain error, when the lookup failed for a reason other
	// than the secret simply being absent.
	Cause error
}

func (e *MissingKeyError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "no %s API key found", e.Provider)
	if e.Cause != nil {
		fmt.Fprintf(&b, " (keychain: %v)", e.Cause)
	}
	fmt.Fprintf(&b, "\n  set it in the keychain:  security add-generic-password -s %s -a %s -w",
		e.Service, KeychainUser)
	fmt.Fprintf(&b, "\n  or export it:            export %s=...", e.EnvVar)
	return b.String()
}

// UserFixable marks this as an environment problem the user can correct, as
// opposed to an internal failure.
func (e *MissingKeyError) UserFixable() bool { return true }

func (e *MissingKeyError) Unwrap() error { return e.Cause }

// providerCredential describes where a provider's key lives.
type providerCredential struct {
	service string
	envVar  string
}

var credentials = map[string]providerCredential{
	"anthropic": {service: "apex:anthropic", envVar: "ANTHROPIC_API_KEY"},
	"openai":    {service: "apex:openai", envVar: "OPENAI_API_KEY"},
}

// UnknownProviderError is an internal error: the caller asked for a provider
// Apex has no credential mapping for.
type UnknownProviderError struct{ Provider string }

func (e *UnknownProviderError) Error() string {
	return fmt.Sprintf("unknown provider %q (known: anthropic, openai)", e.Provider)
}

// CredentialLocation returns the keychain service and environment variable
// consulted for a provider, for display by `apex doctor`.
func CredentialLocation(provider string) (service, envVar string, err error) {
	c, ok := credentials[provider]
	if !ok {
		return "", "", &UnknownProviderError{Provider: provider}
	}
	return c.service, c.envVar, nil
}

// ResolveKey finds the API key for a provider, in the order given by
// DESIGN.md §13: keychain first, environment second, explicit error third.
func ResolveKey(ctx context.Context, provider string) (Key, error) {
	if err := ctx.Err(); err != nil {
		return Key{}, err
	}
	cred, ok := credentials[provider]
	if !ok {
		return Key{}, &UnknownProviderError{Provider: provider}
	}

	secret, keychainErr := keyring.Get(cred.service, KeychainUser)
	if keychainErr == nil {
		if v := strings.TrimSpace(secret); v != "" {
			return Key{Provider: provider, Source: SourceKeychain, Origin: cred.service, value: v}, nil
		}
	}

	if v := strings.TrimSpace(os.Getenv(cred.envVar)); v != "" {
		return Key{Provider: provider, Source: SourceEnv, Origin: cred.envVar, value: v}, nil
	}

	miss := &MissingKeyError{Provider: provider, Service: cred.service, EnvVar: cred.envVar}
	// A keychain that is present but failing (locked, access denied) is worth
	// surfacing; a plain "not found" is not an error worth repeating.
	if keychainErr != nil && !errors.Is(keychainErr, keyring.ErrNotFound) {
		miss.Cause = fmt.Errorf("read %s: %w", cred.service, keychainErr)
	}
	return Key{}, miss
}
