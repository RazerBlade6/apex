package claudecode

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/RazerBlade6/apex/internal/executor"
)

// runLog tees a dispatch to ~/.apex/logs/exec/<run-id>.log (DESIGN.md §5, §9).
//
// A nil file means logging is disabled, and every method is a no-op — which is
// what a caller that only wants the event stream gets, and what keeps a
// failure to open the log from being a failure to dispatch.
//
// Write errors are dropped deliberately. The log is a record of the run, not
// the run itself, and a full disk must not kill work the agent is part-way
// through; the terminal still has the whole event stream.
type runLog struct {
	f *os.File
	w *bufio.Writer
}

func (l *runLog) writer() *bufio.Writer {
	if l == nil || l.f == nil {
		return nil
	}
	if l.w == nil {
		l.w = bufio.NewWriterSize(l.f, 64<<10)
	}
	return l.w
}

// header records what was dispatched, before anything is dispatched.
//
// The full brief goes in. It is the single most useful thing in the file: a
// run that did the wrong thing is almost always a run that was asked for the
// wrong thing, and without this the log shows only the answer.
//
// argv is recorded except for the brief itself, which follows in readable
// form rather than as one enormous quoted line.
func (l *runLog) header(bin string, args []string, brief executor.TaskBrief, at time.Time) {
	w := l.writer()
	if w == nil {
		return
	}
	fmt.Fprintf(w, "# apex exec run %s\n", brief.RunID)
	fmt.Fprintf(w, "# started    %s\n", at.Format(time.RFC3339))
	fmt.Fprintf(w, "# executor   %s (%s)\n", Name, bin)
	if brief.ActionItemID != "" {
		fmt.Fprintf(w, "# item       %s\n", brief.ActionItemID)
	}
	fmt.Fprintf(w, "# project    %s\n", brief.ProjectPath)
	if brief.SessionID != "" {
		fmt.Fprintf(w, "# session    %s\n", brief.SessionID)
		fmt.Fprintf(w, "# resume     claude --resume %s\n", brief.SessionID)
	}
	// Folded: a recorded argument that contains newlines would otherwise
	// continue past the comment prefix and make the header unreadable.
	fmt.Fprintf(w, "# argv       %s\n",
		strings.ReplaceAll(strings.Join(redactBrief(args), " "), "\n", " ⏎ "))
	w.WriteString("#\n# --- brief ---\n")
	for _, line := range strings.Split(strings.TrimRight(brief.Instruction, "\n"), "\n") {
		fmt.Fprintf(w, "# %s\n", line)
	}
	w.WriteString("# --- stream ---\n")
	w.Flush() //nolint:errcheck // see the type comment
}

// redactBrief replaces the appended system prompt in a recorded argv with a
// placeholder, because it is written out in full immediately below and a
// multi-kilobyte single line helps nobody.
func redactBrief(args []string) []string {
	out := make([]string, 0, len(args))
	skip := false
	for _, a := range args {
		if skip {
			out = append(out, "<brief, below>")
			skip = false
			continue
		}
		if a == "--append-system-prompt" {
			out = append(out, a)
			skip = true
			continue
		}
		out = append(out, a)
	}
	return out
}

// line records one line of the agent's stdout, verbatim.
func (l *runLog) line(raw string) {
	if w := l.writer(); w != nil {
		w.WriteString(raw)
		w.WriteByte('\n')
	}
}

// stderr records whatever the agent wrote to stderr, which is where a crash
// explains itself.
func (l *runLog) stderr(tail string) {
	w := l.writer()
	if w == nil {
		return
	}
	w.WriteString("# --- stderr ---\n")
	for _, line := range strings.Split(strings.TrimRight(tail, "\n"), "\n") {
		fmt.Fprintf(w, "# %s\n", line)
	}
}

// footer records the outcome.
func (l *runLog) footer(status string, elapsed time.Duration, detail string) {
	w := l.writer()
	if w == nil {
		return
	}
	fmt.Fprintf(w, "# --- %s after %s ---\n", status, elapsed.Round(time.Second))
	if detail != "" {
		for _, line := range strings.Split(strings.TrimRight(detail, "\n"), "\n") {
			fmt.Fprintf(w, "# %s\n", line)
		}
	}
}

// Close flushes and closes the log.
func (l *runLog) Close() {
	if l == nil || l.f == nil {
		return
	}
	if l.w != nil {
		l.w.Flush() //nolint:errcheck // see the type comment
	}
	l.f.Close() //nolint:errcheck
	l.f = nil
}
