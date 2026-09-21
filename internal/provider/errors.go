package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

// Kind classifies a provider failure.
//
// DESIGN.md §8 is explicit that one broad error class is not good enough: the
// retryable failures (429, 5xx, a connection that never landed) and the
// non-retryable ones (400, 404, a rejected key) call for different responses,
// and `apex doctor --probe` has to tell a user which of them happened.
type Kind int

const (
	// KindUnknown is a failure Apex could not classify. It is the only kind
	// that suggests a bug in Apex rather than in the environment.
	KindUnknown Kind = iota
	// KindAuth is 401 or 403: the key is absent, wrong, or not permitted.
	KindAuth
	// KindRateLimit is 429: the key works, the account is over its limit.
	KindRateLimit
	// KindRequest is 400, 404, or 422: the request itself is wrong — a model
	// id that does not exist, a schema the vendor rejects.
	KindRequest
	// KindServer is any 5xx: the vendor's problem, worth retrying.
	KindServer
	// KindNetwork is a failure before any HTTP response arrived.
	KindNetwork
	// KindCancelled is the caller's context ending, not a failure at all.
	KindCancelled
)

func (k Kind) String() string {
	switch k {
	case KindAuth:
		return "auth"
	case KindRateLimit:
		return "rate_limit"
	case KindRequest:
		return "request"
	case KindServer:
		return "server"
	case KindNetwork:
		return "network"
	case KindCancelled:
		return "cancelled"
	default:
		return "unknown"
	}
}

// Retryable reports whether the same request could succeed if sent again.
func (k Kind) Retryable() bool {
	switch k {
	case KindRateLimit, KindServer, KindNetwork:
		return true
	default:
		return false
	}
}

// Error is a provider failure, classified.
//
// Message holds the vendor's own error body, truncated. It never holds the API
// key: the SDKs keep credentials in request headers, and Apex only ever reads
// the response body and the request URL. Nothing here calls the SDKs'
// DumpRequest helpers, which would serialize the x-api-key header.
type Error struct {
	Provider   string
	Op         string // "stream" | "structured"
	Kind       Kind
	StatusCode int    // 0 when no HTTP response arrived
	Message    string // vendor error body, truncated
	Err        error
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: %s", e.Provider, e.Op, e.Kind)
	if e.StatusCode != 0 {
		fmt.Fprintf(&b, " (HTTP %d)", e.StatusCode)
	}
	switch {
	case e.Message != "":
		fmt.Fprintf(&b, ": %s", e.Message)
	case e.Err != nil:
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// Retryable reports whether the same request could succeed if sent again.
func (e *Error) Retryable() bool { return e.Kind.Retryable() }

// UserFixable marks everything except an unclassifiable failure as an
// environment problem rather than a bug in Apex: a rejected key, a model id
// that does not exist, an unreachable network and a rate limit are all things
// the user acts on. main.go uses this to decide whether to suggest `apex
// doctor`.
func (e *Error) UserFixable() bool { return e.Kind != KindUnknown }

// StatusKind classifies an HTTP status code.
func StatusKind(code int) Kind {
	switch {
	case code == 401 || code == 403:
		return KindAuth
	case code == 429:
		return KindRateLimit
	case code == 400 || code == 404 || code == 422:
		return KindRequest
	case code >= 500:
		return KindServer
	case code >= 400:
		return KindRequest
	default:
		return KindUnknown
	}
}

// TransportKind classifies a failure that carries no HTTP status: a cancelled
// context, or a connection that never landed.
func TransportKind(err error) Kind {
	switch {
	case err == nil:
		return KindUnknown
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return KindCancelled
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return KindNetwork
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return KindNetwork
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return KindNetwork
	}
	return KindUnknown
}

// maxMessageLen bounds what a vendor error body contributes to an error
// string. A proxy answering 401 with an HTML page must not fill the terminal.
const maxMessageLen = 300

// Truncate shortens a vendor message for display.
func Truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxMessageLen {
		return s
	}
	return s[:maxMessageLen] + "…"
}
