package tui

import (
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// Styling and markdown rendering.
//
// Glamour is used here and deliberately nowhere else. DESIGN.md §18 left the
// decision to M6 and cmd/apex/render.go states the constraint it has to
// respect: the non-interactive commands emit no ANSI, so `apex show AI-003 >
// note.md` produces a file rather than escape sequences. That property is
// worth more than styled output in a pipe, so the plain renderer stays where
// it is and Glamour renders only into the TUI's own scrollback, which is never
// redirected anywhere.

// Colours are named from the 256-colour palette rather than truecolor hex, so
// the application degrades sensibly in a terminal that has fewer.
var (
	colAccent = lipgloss.Color("39")  // the active tab, the user's own turns
	colDim    = lipgloss.Color("244") // chrome
	colWarn   = lipgloss.Color("214")
	colError  = lipgloss.Color("203")
	colOK     = lipgloss.Color("78")
)

var (
	styleTab       = lipgloss.NewStyle().Foreground(colDim)
	styleTabActive = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(colAccent).Bold(true)
	styleDim       = lipgloss.NewStyle().Foreground(colDim)
	styleWarn      = lipgloss.NewStyle().Foreground(colWarn)
	styleError     = lipgloss.NewStyle().Foreground(colError)
	styleOK        = lipgloss.NewStyle().Foreground(colOK)
	styleAccent    = lipgloss.NewStyle().Foreground(colAccent)
	styleSelected  = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Bold(true)
	styleLabel     = lipgloss.NewStyle().Foreground(colAccent).Bold(true)
)

// minWidth and maxWidth bound the wrap. The floor keeps a narrow pane
// readable; the ceiling exists because prose measurably stops being readable
// somewhere past a hundred columns, and a full-screen terminal is wider.
const (
	minContentWidth = 40
	maxContentWidth = 100
)

func contentWidth(total int) int {
	w := total - 2
	if w < minContentWidth {
		return minContentWidth
	}
	if w > maxContentWidth {
		return maxContentWidth
	}
	return w
}

// renderer wraps Glamour.
//
// It holds a nil TermRenderer when Glamour could not be constructed — an
// unknown style, a terminal it cannot profile — and falls back to the raw
// markdown. Styling is the least important thing on the screen; a chat view
// that refused to display a reply because it could not colour it would be a
// worse failure than an unstyled one.
type renderer struct {
	tr    *glamour.TermRenderer
	width int
}

// newRenderer builds the renderer the application uses.
//
// WithAutoStyle picks light or dark from the terminal, and falls back to an
// unstyled "notty" profile when there is no terminal to ask — which is exactly
// right: a TUI launched with its output redirected should not be writing
// escape sequences into a file. It does mean styling cannot be asserted
// without naming a style, which is what buildRenderer is for.
func newRenderer(width int) *renderer {
	return buildRenderer(width, glamour.WithAutoStyle())
}

func buildRenderer(width int, style glamour.TermRendererOption) *renderer {
	tr, err := glamour.NewTermRenderer(
		style,
		glamour.WithWordWrap(width),
		glamour.WithEmoji(),
	)
	if err != nil {
		return &renderer{width: width}
	}
	return &renderer{tr: tr, width: width}
}

// markdown renders a block of markdown for the scrollback.
func (r *renderer) markdown(src string) string {
	src = strings.TrimSpace(src)
	if src == "" {
		return ""
	}
	if r.tr == nil {
		return wrapPlain(src, r.width)
	}
	out, err := r.tr.Render(src)
	if err != nil {
		return wrapPlain(src, r.width)
	}
	// Glamour pads generously at both ends; in a scrollback of many short
	// turns that reads as a gap rather than as breathing room.
	return strings.Trim(out, "\n")
}

// wrapPlain is the fallback: hard-wrap on whitespace, no styling.
func wrapPlain(src string, width int) string {
	var b strings.Builder
	for i, line := range strings.Split(src, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(wrapLine(line, width))
	}
	return b.String()
}

func wrapLine(line string, width int) string {
	words := strings.Fields(line)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	col := 0
	for i, w := range words {
		n := runewidth.StringWidth(w)
		switch {
		case i == 0:
			b.WriteString(w)
			col = n
		case col+1+n <= width:
			b.WriteByte(' ')
			b.WriteString(w)
			col += 1 + n
		default:
			b.WriteByte('\n')
			b.WriteString(w)
			col = n
		}
	}
	return b.String()
}

// truncate cuts a single line to width, by display cells rather than bytes.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= width {
		return s
	}
	return runewidth.Truncate(s, width, "…")
}

// padBetween puts left and right on one line of the given width, with the
// space between them. It measures rendered width, so styled text lines up.
func padBetween(left, right string, width int) string {
	lw := lipgloss.Width(left)
	rw := lipgloss.Width(right)
	if lw+rw+1 > width {
		return truncate(left, width)
	}
	return left + strings.Repeat(" ", width-lw-rw) + right
}

// padLines makes a block exactly n lines tall, so the frame does not jump as
// content grows.
func padLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	for len(lines) < n {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}
