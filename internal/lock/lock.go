// Package lock provides machine-local advisory file locks (DESIGN.md §10).
//
// flock(2) is the primitive because the kernel releases the lock when the
// holding process dies. There is deliberately no stale-lock detection, no
// timeout heuristic, and no PID liveness check: the failure mode that makes
// hand-rolled lockfiles unreliable does not arise here.
package lock

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Mode selects the kind of lock taken.
type Mode int

const (
	// Exclusive is taken by anything that mutates the resource: a dispatch
	// against a project working tree, or a replica Sync.
	Exclusive Mode = iota
	// Shared is taken by readers. Several may hold it at once, but none may
	// hold it while an Exclusive lock is held.
	Shared
)

func (m Mode) String() string {
	if m == Shared {
		return "shared"
	}
	return "exclusive"
}

// Lock is a held flock. It is released by Release, or by the process exiting.
type Lock struct {
	path  string
	runID string
	mode  Mode
	f     *os.File
}

// Path returns the lockfile path.
func (l *Lock) Path() string { return l.path }

// RunID returns the run identifier written into the lockfile.
func (l *Lock) RunID() string { return l.runID }

// Mode returns the mode this lock was acquired in.
func (l *Lock) Mode() Mode { return l.mode }

// BusyError reports that the lock is already held, and carries the holder
// string read from the lockfile so the message can say what is running rather
// than just "locked".
type BusyError struct {
	Path   string
	Holder string
}

func (e *BusyError) Error() string {
	if h := strings.TrimSpace(e.Holder); h != "" {
		return fmt.Sprintf("lock %s is held: %s", e.Path, h)
	}
	return fmt.Sprintf("lock %s is held", e.Path)
}

// UserFixable marks contention as a condition the user resolves (by waiting or
// stopping the other run), not an internal failure.
func (e *BusyError) UserFixable() bool { return true }

// Holder describes the process recorded in a lockfile.
type Holder struct {
	RunID   string
	PID     int
	Started time.Time
}

// ParseHolder reads the `run=<id> pid=<n> started=<rfc3339>` line Acquire
// writes. Fields it cannot parse are left zero, since the holder string is
// best-effort diagnostic data.
func ParseHolder(s string) Holder {
	var h Holder
	for _, field := range strings.Fields(s) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch key {
		case "run":
			h.RunID = value
		case "pid":
			var pid int
			if _, err := fmt.Sscanf(value, "%d", &pid); err == nil {
				h.PID = pid
			}
		case "started":
			if t, err := time.Parse(time.RFC3339, value); err == nil {
				h.Started = t
			}
		}
	}
	return h
}

// Holder parses the holder recorded on a BusyError.
func (e *BusyError) HolderInfo() Holder { return ParseHolder(e.Holder) }

// NewRunID returns a short random identifier suitable for a lockfile and an
// exec_runs row.
func NewRunID() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A failing CSPRNG is not worth failing a lock over; the run ID is
		// diagnostic, not a security boundary.
		return fmt.Sprintf("%06x", time.Now().UnixNano()&0xffffff)
	}
	return hex.EncodeToString(b[:])
}

// Acquire takes a lock on path with a freshly generated run ID. It is
// non-blocking: a held lock returns *BusyError naming the holder.
func Acquire(path string, mode Mode) (*Lock, error) {
	return AcquireAs(path, mode, NewRunID())
}

// AcquireAs is Acquire with a caller-supplied run ID, so a dispatch can use the
// same identifier for its lock, its exec_runs row, and its log file.
//
// DESIGN.md §10 shows Acquire(path, mode) using a runID from its enclosing
// scope; AcquireAs is that function with the identifier passed in explicitly.
func AcquireAs(path string, mode Mode, runID string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock directory for %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}

	how := unix.LOCK_EX | unix.LOCK_NB
	if mode == Shared {
		how = unix.LOCK_SH | unix.LOCK_NB
	}
	if err := unix.Flock(int(f.Fd()), how); err != nil {
		holder, _ := io.ReadAll(f) // best effort; empty is fine
		f.Close()
		return nil, &BusyError{Path: path, Holder: string(holder)}
	}

	// Record who holds it. A shared lock may be held by several processes at
	// once, so the last writer wins; that is acceptable for a diagnostic.
	if err := f.Truncate(0); err != nil {
		releaseFile(f)
		return nil, fmt.Errorf("truncate lock %s: %w", path, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		releaseFile(f)
		return nil, fmt.Errorf("rewind lock %s: %w", path, err)
	}
	if _, err := fmt.Fprintf(f, "run=%s pid=%d started=%s",
		runID, os.Getpid(), time.Now().Format(time.RFC3339)); err != nil {
		releaseFile(f)
		return nil, fmt.Errorf("write lock holder to %s: %w", path, err)
	}

	return &Lock{path: path, runID: runID, mode: mode, f: f}, nil
}

// Release unlocks and closes the lockfile. Closing the descriptor would be
// enough; the explicit unlock keeps the intent visible.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := releaseFile(l.f)
	l.f = nil
	if err != nil {
		return fmt.Errorf("release lock %s: %w", l.path, err)
	}
	return nil
}

func releaseFile(f *os.File) error {
	unlockErr := unix.Flock(int(f.Fd()), unix.LOCK_UN)
	closeErr := f.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
