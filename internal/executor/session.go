package executor

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// NewSessionID returns a random RFC 4122 version 4 UUID.
//
// DESIGN.md §9: "Apex generates the session UUID and stores it on the
// exec_runs row. A stalled or failed dispatch is then resumable with `claude
// --resume <uuid>`, and the run is traceable after the fact." Generating it
// here rather than letting the agent pick one is what makes that true — an
// id Apex never saw is an id nobody can resume.
//
// It is written out rather than taken from github.com/google/uuid, which is
// already in the module graph as an indirect dependency: sixteen bytes and a
// hyphen pattern is not worth promoting a dependency to direct, and this is
// the only place in Apex that needs a UUID.
func NewSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Unlike a lock's run id, this one cannot fall back to a timestamp:
		// the CLI validates the shape, and a session id that collides with
		// another run's would attach this dispatch to that transcript.
		return "", fmt.Errorf("executor: generate a session id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32], nil
}
