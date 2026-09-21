package lock

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func lockPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "locks", "scholarrag.lock")
}

// TestAcquireCombinations covers the compatibility matrix. flock is held per
// open file description, so two Acquire calls in one process contend exactly as
// two processes would.
func TestAcquireCombinations(t *testing.T) {
	tests := []struct {
		name      string
		first     Mode
		second    Mode
		wantBusy  bool
		afterFree bool // whether the second acquire should succeed once the first is released
	}{
		{name: "exclusive then exclusive", first: Exclusive, second: Exclusive, wantBusy: true, afterFree: true},
		{name: "exclusive then shared", first: Exclusive, second: Shared, wantBusy: true, afterFree: true},
		{name: "shared then exclusive", first: Shared, second: Exclusive, wantBusy: true, afterFree: true},
		{name: "shared then shared", first: Shared, second: Shared, wantBusy: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := lockPath(t)

			first, err := Acquire(path, tt.first)
			if err != nil {
				t.Fatalf("first Acquire(%s): %v", tt.first, err)
			}

			second, err := Acquire(path, tt.second)
			if !tt.wantBusy {
				if err != nil {
					t.Fatalf("second Acquire(%s): %v", tt.second, err)
				}
				if err := second.Release(); err != nil {
					t.Errorf("Release second: %v", err)
				}
				if err := first.Release(); err != nil {
					t.Errorf("Release first: %v", err)
				}
				return
			}

			var busy *BusyError
			if !errors.As(err, &busy) {
				t.Fatalf("second Acquire(%s) error = %v, want *BusyError", tt.second, err)
			}
			if busy.Path != path {
				t.Errorf("BusyError.Path = %q, want %q", busy.Path, path)
			}
			if !busy.UserFixable() {
				t.Error("BusyError should be user-fixable")
			}
			if !strings.Contains(busy.Holder, "run="+first.RunID()) {
				t.Errorf("BusyError.Holder = %q, want it to name run %s", busy.Holder, first.RunID())
			}

			if tt.afterFree {
				if err := first.Release(); err != nil {
					t.Fatalf("Release first: %v", err)
				}
				retry, err := Acquire(path, tt.second)
				if err != nil {
					t.Fatalf("Acquire after Release: %v", err)
				}
				if err := retry.Release(); err != nil {
					t.Errorf("Release retry: %v", err)
				}
			}
		})
	}
}

func TestHolderRecordedInFile(t *testing.T) {
	path := lockPath(t)

	l, err := AcquireAs(path, Exclusive, "7f3a91")
	if err != nil {
		t.Fatalf("AcquireAs: %v", err)
	}
	t.Cleanup(func() { _ = l.Release() })

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lockfile: %v", err)
	}
	h := ParseHolder(string(body))
	if h.RunID != "7f3a91" {
		t.Errorf("RunID = %q, want 7f3a91 (raw: %q)", h.RunID, body)
	}
	if h.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d (raw: %q)", h.PID, os.Getpid(), body)
	}
	if time.Since(h.Started) > time.Minute || h.Started.IsZero() {
		t.Errorf("Started = %v, want a fresh timestamp (raw: %q)", h.Started, body)
	}
	if l.Mode() != Exclusive || l.Path() != path {
		t.Errorf("Lock = {%s %s}, want {exclusive %s}", l.Mode(), l.Path(), path)
	}
}

// A second acquire must overwrite the previous holder, not append to it.
func TestHolderIsTruncatedOnReacquire(t *testing.T) {
	path := lockPath(t)

	first, err := AcquireAs(path, Exclusive, "aaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}

	second, err := AcquireAs(path, Exclusive, "bbbb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Release() })

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "aaaa") {
		t.Errorf("lockfile still carries the previous holder: %q", body)
	}
	if got := ParseHolder(string(body)).RunID; got != "bbbb" {
		t.Errorf("RunID = %q, want bbbb (raw: %q)", got, body)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	l, err := Acquire(lockPath(t), Exclusive)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Errorf("second Release: %v, want nil", err)
	}
}

func TestParseHolder(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  Holder
	}{
		{
			name:  "full line",
			input: "run=7f3a91 pid=8821 started=2026-09-21T10:04:05Z",
			want:  Holder{RunID: "7f3a91", PID: 8821, Started: time.Date(2026, 9, 21, 10, 4, 5, 0, time.UTC)},
		},
		{"empty", "", Holder{}},
		{"garbage", "held", Holder{}},
		{"partial", "run=abc", Holder{RunID: "abc"}},
		{"bad pid", "run=abc pid=x", Holder{RunID: "abc"}},
		{"bad time", "run=abc started=yesterday", Holder{RunID: "abc"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseHolder(tt.input)
			if got.RunID != tt.want.RunID || got.PID != tt.want.PID || !got.Started.Equal(tt.want.Started) {
				t.Errorf("ParseHolder(%q) = %+v, want %+v", tt.input, got, tt.want)
			}
		})
	}
}

func TestNewRunIDIsDistinct(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := NewRunID()
		if id == "" {
			t.Fatal("NewRunID returned an empty string")
		}
		seen[id] = true
	}
	if len(seen) < 90 {
		t.Errorf("NewRunID produced %d distinct values out of 100", len(seen))
	}
}

func TestAcquireCreatesLockDirectory(t *testing.T) {
	// ~/.apex/locks may not exist on a first run.
	path := filepath.Join(t.TempDir(), "deeply", "nested", "db.lock")
	l, err := Acquire(path, Shared)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()

	if _, err := os.Stat(path); err != nil {
		t.Errorf("lockfile was not created: %v", err)
	}
}
