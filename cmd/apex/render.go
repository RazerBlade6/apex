package main

import (
	"io"
	"os"
	"strconv"
	"strings"
)

// Markdown rendering for the terminal.
//
// DESIGN.md §3 names Glamour as the markdown renderer, and M6 added it — for
// internal/tui's chat scrollback, and for nothing else.
//
// This renderer stays, and the two are not a duplication waiting to be
// consolidated: one exists precisely because the other emits ANSI. Everything
// below writes plain text, so `apex show AI-003 > note.md` produces a file
// rather than a screenful of escape sequences, and every command here is meant
// to be cron-able and pipeable (§11).
//
// What these listings need is narrow: wrap prose to the terminal, keep list
// structure, and do not mangle a fenced code block.

// defaultWidth is used when the terminal's width is unknown. 80 is the
// conventional floor and is narrow enough to read comfortably in a wide
// window.
const defaultWidth = 80

// maxWidth keeps a full-screen terminal from producing 200-character lines,
// which are measurably harder to read than wrapped prose.
const maxWidth = 96

// terminalWidth reports how wide to wrap.
//
// $COLUMNS is consulted rather than an ioctl because the ioctl needs
// golang.org/x/term, and the difference between a correct width and a
// reasonable one does not justify a dependency. Shells that export COLUMNS get
// the exact value; everything else gets 80.
func terminalWidth() int {
	if v := os.Getenv("COLUMNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 40 {
			return min(n, maxWidth)
		}
	}
	return defaultWidth
}

// renderMarkdown writes markdown as readable plain text, indented by `indent`.
func renderMarkdown(w io.Writer, src string, indent string, width int) {
	body := strings.ReplaceAll(src, "\r\n", "\n")
	lines := strings.Split(body, "\n")

	inFence := false
	pending := false // a blank line is owed before the next block

	flushBlank := func() {
		if pending {
			io.WriteString(w, "\n") //nolint:errcheck // terminal writes
			pending = false
		}
	}

	var para []string
	flushPara := func() {
		if len(para) == 0 {
			return
		}
		flushBlank()
		// The hanging prefix is the same indent, not "": a continuation line
		// that loses it puts the second line of an indented paragraph hard
		// against the left margin.
		writeWrapped(w, strings.Join(para, " "), indent, indent, width)
		para = para[:0]
		pending = true
	}

	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "```") {
			flushPara()
			inFence = !inFence
			continue
		}
		if inFence {
			flushBlank()
			// Verbatim: wrapping code changes what it means.
			io.WriteString(w, indent+"    "+line+"\n") //nolint:errcheck
			continue
		}
		if trimmed == "" {
			flushPara()
			continue
		}
		if heading, ok := strings.CutPrefix(trimmed, "#"); ok {
			flushPara()
			flushBlank()
			text := strings.TrimSpace(strings.TrimLeft(heading, "#"))
			io.WriteString(w, indent+inline(text)+"\n") //nolint:errcheck
			pending = true
			continue
		}
		if marker, rest, ok := listMarker(line); ok {
			flushPara()
			flushBlank()
			writeWrapped(w, inline(rest), indent+marker, indent+strings.Repeat(" ", len(marker)), width)
			continue
		}
		para = append(para, inline(trimmed))
	}
	flushPara()
}

// listMarker splits a list line into its bullet and its text, preserving the
// original indentation so nested lists stay nested.
func listMarker(line string) (marker, rest string, ok bool) {
	lead := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	body := strings.TrimLeft(line, " \t")
	lead = strings.ReplaceAll(lead, "\t", "    ")

	for _, bullet := range []string{"- ", "* ", "+ "} {
		if after, found := strings.CutPrefix(body, bullet); found {
			return lead + "- ", after, true
		}
	}
	// An ordered item: digits, then "." or ")", then a space.
	i := 0
	for i < len(body) && body[i] >= '0' && body[i] <= '9' {
		i++
	}
	if i > 0 && i+1 < len(body) && (body[i] == '.' || body[i] == ')') && body[i+1] == ' ' {
		return lead + body[:i+2], body[i+2:], true
	}
	return "", "", false
}

// inline strips the markup that reads as noise in a terminal. Emphasis markers
// carry no meaning once there is no styling to apply; backticks are kept,
// because in plain text they are the only thing marking an identifier.
func inline(s string) string {
	s = strings.ReplaceAll(s, "**", "")
	s = strings.ReplaceAll(s, "__", "")
	return s
}

// writeWrapped writes text wrapped to width, with `first` before the first
// line and `hang` before the rest.
func writeWrapped(w io.Writer, text, first, hang string, width int) {
	words := strings.Fields(text)
	if len(words) == 0 {
		io.WriteString(w, strings.TrimRight(first, " ")+"\n") //nolint:errcheck
		return
	}
	prefix := first
	line := prefix
	started := false
	for _, word := range words {
		switch {
		case !started:
			line += word
			started = true
		case len(line)+1+len(word) <= width:
			line += " " + word
		default:
			io.WriteString(w, line+"\n") //nolint:errcheck
			prefix = hang
			line = prefix + word
		}
	}
	io.WriteString(w, line+"\n") //nolint:errcheck
}
