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

// The palette is gruvbox dark, named as truecolor hex (UI.md §3).
//
// Hex rather than 256-colour indices because lipgloss hands the colour to
// termenv, which downsamples it to the nearest entry the terminal actually
// advertises. Naming the exact gruvbox value therefore gets gruvbox where
// truecolor is available and the closest approximation where it is not, which
// is strictly better than picking an index that is already an approximation
// everywhere. Gruvbox's own fg1 (#ebdbb2) is deliberately absent: primary text
// is left as the terminal's own foreground, so the frame sits on whatever
// background the user has rather than fighting it.
var (
	colBG0Hard = lipgloss.Color("#1d2021") // text on a bright fill
	colBG1     = lipgloss.Color("#3c3836") // dark grey fill
	colBG3     = lipgloss.Color("#665c54") // the border
	colGrey    = lipgloss.Color("#928374") // chrome and dim text
	colFG0     = lipgloss.Color("#fbf1c7") // the brightest text
	colRed     = lipgloss.Color("#fb4934")
	colGreen   = lipgloss.Color("#b8bb26")
	colYellow  = lipgloss.Color("#fabd2f")
	colBlue    = lipgloss.Color("#83a598")
	colOrange  = lipgloss.Color("#fe8019")
)

var (
	styleTab       = lipgloss.NewStyle().Foreground(colGrey)
	styleTabActive = lipgloss.NewStyle().Foreground(colBG0Hard).Background(colBlue).Bold(true)
	styleDim       = lipgloss.NewStyle().Foreground(colGrey)
	// Warnings are orange rather than yellow so that a warning cannot be
	// mistaken for a section label, which is the other thing on screen with a
	// bold warm foreground.
	styleWarn   = lipgloss.NewStyle().Foreground(colOrange)
	styleError  = lipgloss.NewStyle().Foreground(colRed)
	styleOK     = lipgloss.NewStyle().Foreground(colGreen)
	styleAccent = lipgloss.NewStyle().Foreground(colBlue)
	// styleSelected carries a background, so every row it renders has to be
	// padded to the full inner width first: a highlight that stops at the end
	// of the text has a ragged right edge and reads as a rendering fault
	// rather than as a cursor.
	styleSelected = lipgloss.NewStyle().Foreground(colFG0).Background(colBG1).Bold(true)
	styleLabel    = lipgloss.NewStyle().Foreground(colYellow).Bold(true)

	// styleBox is the bounding box around the content area. Width is set per
	// frame, because it depends on the terminal.
	styleBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colBG3).
			Padding(0, 1)

	// styleBoxRight is the chat view's output pane. The one column between the
	// two panes is its left margin rather than a spacer string, so that
	// JoinHorizontal cannot lose it — and chatPaneWidths counts it, because a
	// margin widens the rendered block.
	styleBoxRight = styleBox.MarginLeft(paneGap)
)

// minWidth and maxWidth bound the wrap. The floor keeps a narrow pane readable.
// The ceiling used to be 100, on the reasoning that prose stops being readable
// somewhere past a hundred columns — true of a single column of prose, but the
// chat view no longer is one. Apex's replies get roughly two thirds of the
// width and the prompts the rest, so the ceiling that matters for reading is
// the pane's, not the frame's. At 100 the frame occupied about 40% of a wide
// display and looked marooned in the middle of it.
const (
	minContentWidth = 40
	maxContentWidth = 200
)

// boxChrome is what the bounding box costs horizontally: two border columns
// and two padding columns. It is the difference between the inner text width
// and the box's rendered width.
const boxChrome = 4

// boxGutter is one side of that chrome: a border column and a padding column.
// The rows that sit outside the box are indented by it so that their text
// starts in the same column as the text inside the box.
const boxGutter = boxChrome / 2

// The chat view's two panes (UI.md §2). paneChrome is what one pane's
// border and padding cost horizontally — the same four columns as boxChrome,
// named separately because it is paid twice over — and paneGap is the single
// column between them. The split is roughly a third to the prompts and the rest
// to Apex's output, bounded so the left pane is neither unreadably narrow nor
// wider than a prompt needs on a very wide terminal. maxPaneInner tracks
// maxContentWidth: left at 34 while the frame doubled, every new column would
// land in the right pane and the third-to-two-thirds split would quietly become
// a sixth to five sixths.
const (
	paneChrome   = boxChrome
	paneGap      = 1
	paneSplit    = 0.32
	minPaneInner = 18
	maxPaneInner = 68

	// inputBoxHeight is the rendered height of the input's own box inside the
	// left pane: two border rows around the three-row textarea.
	inputBoxHeight = 5

	// minTwoPaneWidth and minTwoPaneHeight are where the side-by-side layout
	// stops being one, and chat falls back to a single box.
	minTwoPaneWidth  = 60
	minTwoPaneHeight = 8
)

// styleGutter indents the status line and the footer to that column.
//
// Aligning them with the box's outer edge instead looks right on the left and
// wrong on the right: the footer's right-hand half would stop two columns
// short of the border on each side, which reads as a rendering slip rather
// than as a margin.
var styleGutter = lipgloss.NewStyle().PaddingLeft(boxGutter)

// contentWidth is the inner text width: the columns a line inside the box
// actually gets, with the border and the padding already deducted.
func contentWidth(total int) int {
	w := total - boxChrome
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

// padTo pads a line out to exactly width display cells.
//
// It exists for the rows that are rendered with a background: without it the
// highlight ends where the text does. Width is measured in cells rather than
// bytes, and rendered cells rather than raw ones, so a styled or double-width
// line still lines up.
func padTo(s string, width int) string {
	gap := width - lipgloss.Width(s)
	if gap <= 0 {
		return s
	}
	return s + strings.Repeat(" ", gap)
}

// glamourMargin is the two-column margin Glamour puts on a document.
//
// It is a constant here because it cannot be reached through the options
// Glamour exposes: WithAutoStyle resolves the light or dark config through an
// unexported helper, so overriding the margin would mean reimplementing the
// terminal background detection to rebuild the config by hand. Matching the
// margin is cheaper and safer than fighting it, so the plain-text blocks that
// share a pane with rendered markdown are wrapped narrower and indented by it.
const glamourMargin = 2

// indentBlock shifts every line of a block right by n columns, leaving blank
// lines alone so they do not become trailing whitespace.
func indentBlock(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	pad := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l == "" {
			continue
		}
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
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
