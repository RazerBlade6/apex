package claudecli

import (
	"github.com/RazerBlade6/apex/internal/claudecmd"
	"github.com/RazerBlade6/apex/internal/provider"
)

// The wire shape of `claude --output-format stream-json` lives in
// internal/claudecmd, together with the binary lookup and the process-group
// control this package also needs.
//
// It moved there in M5 and the reason is DESIGN.md §18: the executor resolved
// the claude binary its own way, "they agree today", and M5 was asked to
// collapse them so they cannot drift. The same argument applies to the event
// parsing, because M5's executor reads the same stream from the same binary —
// with tools enabled rather than disabled, which changes the command line
// completely and the wire format not at all. One decoder, two readers.
//
// The aliases below keep this package's own code reading the way M3.5 wrote
// it. They are aliases rather than wrappers so that the methods on the
// underlying types — Permissive, Resets, Text, ServingModel — come across
// unchanged.
type (
	line          = claudecmd.Line
	rateLimitInfo = claudecmd.RateLimitInfo
	tailBuffer    = claudecmd.TailBuffer
)

// maxLineBytes bounds one line of stdout; see claudecmd.MaxLineBytes.
const maxLineBytes = claudecmd.MaxLineBytes

// usageOf converts the CLI's accounting to Apex's.
//
// Usage.Model is left empty here and filled by invoke from the serving model
// the run reported, which no single usage block carries: every usage block the
// CLI emits is anonymous.
func usageOf(u *claudecmd.Usage) provider.Usage {
	if u == nil {
		return provider.Usage{}
	}
	return provider.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
}
