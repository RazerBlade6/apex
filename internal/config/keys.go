package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
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

// CredentialKind says how a provider proves who it is.
//
// DESIGN.md §13 was generalised for M3.5: Apex resolves *credentials*, not
// only keys. A route is satisfied either by an API key or by a
// subscription-backed CLI that already holds its own OAuth credential, and
// the difference is not cosmetic — it decides what `doctor` tells the user to
// go and fix. A claude-cli route reported as "missing API key" would send
// them to the keychain to add a key that this provider would never read.
type CredentialKind string

const (
	// CredentialAPIKey is a key Apex resolves and passes to an SDK.
	CredentialAPIKey CredentialKind = "api_key"

	// CredentialSubscription is a credential Apex never sees. The claude CLI
	// holds a Claude Pro or Max OAuth token in its own keychain entry; Apex
	// neither reads, stores, nor forwards it, and only observes — through
	// `claude auth status` — that the CLI has one.
	CredentialSubscription CredentialKind = "subscription"
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

// providerCredential describes how a provider is authenticated, and for a
// key-based one, where its key lives.
type providerCredential struct {
	kind    CredentialKind
	service string // keychain service; empty for a subscription provider
	envVar  string // environment fallback; empty for a subscription provider
}

var credentials = map[string]providerCredential{
	"anthropic": {kind: CredentialAPIKey, service: "apex:anthropic", envVar: "ANTHROPIC_API_KEY"},
	"openai":    {kind: CredentialAPIKey, service: "apex:openai", envVar: "OPENAI_API_KEY"},
	// The claude CLI authenticates itself. Apex has nothing to resolve here,
	// which is exactly why this entry exists: without it, claude-cli would
	// fall through to UnknownProviderError and every caller would have to
	// special-case it.
	"claude-cli": {kind: CredentialSubscription},
}

// UnknownProviderError is an internal error: the caller asked for a provider
// Apex has no credential mapping for.
type UnknownProviderError struct{ Provider string }

func (e *UnknownProviderError) Error() string {
	return fmt.Sprintf("unknown provider %q (known: %s)", e.Provider, strings.Join(knownProviders(), ", "))
}

func knownProviders() []string {
	out := make([]string, 0, len(credentials))
	for name := range credentials {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// KeylessProviderError reports an API key being asked for on behalf of a
// provider that has none.
//
// It is deliberately not a MissingKeyError. A caller that cannot tell the two
// apart prints "no claude-cli API key found — set it in the keychain", which
// is advice for a key that would never be read (DESIGN.md §13).
type KeylessProviderError struct {
	Provider string
	Kind     CredentialKind
}

func (e *KeylessProviderError) Error() string {
	return fmt.Sprintf("provider %q authenticates by %s and has no API key to resolve", e.Provider, e.Kind)
}

// CredentialKindOf reports how a provider authenticates.
func CredentialKindOf(provider string) (CredentialKind, error) {
	c, ok := credentials[provider]
	if !ok {
		return "", &UnknownProviderError{Provider: provider}
	}
	return c.kind, nil
}

// UsesAPIKey reports whether a provider is authenticated with a key Apex
// resolves. An unknown provider reports false, so a caller that skips key
// resolution for it will fail later with a better message than "no key".
func UsesAPIKey(provider string) bool {
	c, ok := credentials[provider]
	return ok && c.kind == CredentialAPIKey
}

// KeyProviders lists the providers that have an API key to resolve, sorted.
// It is what `apex doctor` and `apex config` iterate when reporting key
// sources, so a subscription provider never appears in a table of keys.
func KeyProviders() []string {
	out := make([]string, 0, len(credentials))
	for name, c := range credentials {
		if c.kind == CredentialAPIKey {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// CredentialLocation returns the keychain service and environment variable
// consulted for a provider, for display by `apex doctor`.
//
// A subscription provider has neither, and says so with a
// *KeylessProviderError rather than two empty strings a caller might print.
func CredentialLocation(provider string) (service, envVar string, err error) {
	c, ok := credentials[provider]
	if !ok {
		return "", "", &UnknownProviderError{Provider: provider}
	}
	if c.kind != CredentialAPIKey {
		return "", "", &KeylessProviderError{Provider: provider, Kind: c.kind}
	}
	return c.service, c.envVar, nil
}

// ResolveKey finds the API key for a provider, in the order given by
// DESIGN.md §13: keychain first, environment second, explicit error third.
//
// A provider that authenticates some other way comes back as
// *KeylessProviderError, never as *MissingKeyError. Callers that resolve a
// credential rather than a key should branch on UsesAPIKey first.
func ResolveKey(ctx context.Context, provider string) (Key, error) {
	if err := ctx.Err(); err != nil {
		return Key{}, err
	}
	cred, ok := credentials[provider]
	if !ok {
		return Key{}, &UnknownProviderError{Provider: provider}
	}
	if cred.kind != CredentialAPIKey {
		return Key{}, &KeylessProviderError{Provider: provider, Kind: cred.kind}
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
