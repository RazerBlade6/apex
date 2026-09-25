# Apex UI

The visual contract for the terminal interface: the frame and its arithmetic,
the layouts of the chat and items views, and the palette.

This is separate from DESIGN.md because the two change on different clocks.
DESIGN.md is the architecture, and by v1 most of it is settled; how the
interface looks is not, and it moves every time the application is used in
anger. DESIGN.md §12 keeps the parts of `internal/tui` that are architectural
— the downward-import boundary, the rule that a message belongs to the
component that requested it, the streaming pattern — because those constrain
the shape of the code rather than the look of the screen. Everything here is
appearance, and changing it should never require touching DESIGN.md.

Nothing in here is a suggestion. Terminal layout is arithmetic, and the
failure mode of arithmetic that is one column out is not a slightly wrong
margin — it is a border that wraps and takes the whole frame with it.

## 1. The frame

The three tabs sit centred on the top row, generously spaced, and the version
sits at the right-hand end of the footer rather than beside them, because an
element pinned to the right of the tabs would have made "centred" a lie. The
content area below them is drawn inside a rounded border.

That border costs four columns and two rows, and every measurement follows from
it:

| name | value | meaning |
| --- | --- | --- |
| `contentWidth` | `width - 4`, bounded to 40 and 200 | the inner text width |
| `innerWidth` | `contentWidth`, or `width - 4` when narrower | the same, after the narrow-terminal rule below |
| `frameWidth` | `innerWidth + 4` | the box's rendered outer width |
| `bodyHeight` | `height - 7`, floor 3 | the inner rows the box holds |

Every line drawn inside the box is measured against `innerWidth`, never against
the terminal width — that distinction is the single most common way to put a
character through the border.

The rows sum to `height - 1`: tab row, blank line, status line, box, footer, and
one row deliberately unused, because a frame that fills the last row scrolls the
alt screen on some terminals. The assembled block is centred as a unit rather
than row by row, so the status line and the footer cannot drift against the box;
both are then indented by one border column and one padding column, which puts
their text in the same column as the text inside the box and stops the footer's
right-hand half from ending short of the border.

The 40-column floor needs one special rule. Below 44 columns that floor is wider
than the screen, so the box gives up the floor rather than the fit.

The 200-column ceiling was 100 until the frame was measured on a wide display,
where it occupied about 40% of the screen and looked marooned in the middle of
it. The old reasoning — prose stops being readable somewhere past a hundred
columns — is true of a single column of prose, but the chat view is no longer
one: the ceiling that governs reading is the reply pane's, and that is two
thirds of the frame. The pane bound doubled with it, because leaving the left
pane at 34 while the frame doubled would have sent every new column to the right
pane and quietly turned a third-to-two-thirds split into a sixth to five sixths.
At or below a 204-column terminal the frame now fills the screen; above it the
margins grow again.

## 2. The chat view is two boxes

A conversation read as one column gives a two-line question the same measure as
the answer that follows it. So the user's own turns and the input field sit in a
narrow left-hand box, and Apex's replies in a wide right-hand one.

Each box pays the four columns of border and padding that `innerWidth` has
already deducted once, and one column of gap sits between them:

```
leftInner + rightInner = innerWidth - 5
leftInner              = round(0.32 × (innerWidth - 5)), bounded to 18 and 68
rightInner             = (innerWidth - 5) - leftInner
```

`rightInner` is derived by subtraction and never rounded on its own, because two
independent roundings disagree with the frame at some widths. The pair then
renders exactly `frameWidth` wide, which is what keeps chat lined up with Items
and Projects instead of a column adrift from them. The gap is the right box's
left margin rather than a spacer string, so `JoinHorizontal` cannot lose it, and
the sum counts it because a margin widens the rendered block.

Both boxes are as tall as the single one. Inside the left box the input is its
own bordered box pinned to the bottom — two border rows around the three-row
textarea — so the prompts above it get `bodyHeight - 5` rows, and the textarea
itself gets `leftInner - 4` columns, having paid a border and a padding a second
time.

The two scrollbacks are independent and each sticks to its own bottom. Aligning
turn N's prompt with turn N's reply would mean padding one column out to the
other's height, which a streaming reply changes on every token.

**The fallback.** Two panes need room, so below 60 inner columns or 8 body rows
chat reverts to exactly the layout it had before: one box, scrollback above,
input below. At 40 columns the left pane would be ten columns of text, which is
not a layout. The per-turn `you` and `apex` labels survive only in that
fallback, where the two speakers share a column and nothing else tells them
apart; side by side the column says who is speaking, and the label costs a row
to repeat it.

**The trap that no test catches** is the Glamour renderer. It wraps Apex's
replies, which live in the right-hand box, so it has to be built at `rightInner`
rather than at `innerWidth`. Get it wrong and nothing fails — every reply is
simply wrapped to a column it is not displayed in.

**Plain text has to line up with rendered markdown.** Glamour gives a document a
two-column margin, and that margin is not reachable through the options Glamour
exposes: `WithAutoStyle` resolves the light or dark configuration through an
unexported helper, so overriding it would mean reimplementing the terminal
background detection in order to rebuild the configuration by hand. Matching the
margin is cheaper and safer than fighting it, so the blocks that are not
markdown — the system notes, and the reply that is still arriving — are wrapped
two columns narrower and indented by two. That also removes a shift that was
present before anyone looked for it: a streaming reply is plain text while it
arrives and markdown once it lands, so without the indent it jumped two columns
to the right at the moment it completed.

The input's placeholder is switched on the same predicate, because the full
sentence wraps onto two of the three input rows in a narrow left pane, each
carrying its own prompt glyph, which reads as three empty inputs rather than
one.

## 3. The items view is three boxes

An action item is not a line of text. It belongs to a project whose state is
the reason it exists, and it carries a body and a rationale that a one-line row
can only cut off. So the items view is three boxes: on the left, the project the
selected item belongs to; in the middle, the items themselves; on the right, the
selected item in full.

Each box pays `paneChrome` and two `paneGap` columns sit between them, so against
the single box the trio has ten columns fewer to share:

```
leftInner + midInner + rightInner = innerWidth + 4 - 3×4 - 2 = innerWidth - 10
leftInner = rightInner            = round(0.30 × (innerWidth - 10))
midInner                          = (innerWidth - 10) - leftInner - rightInner
```

The sides are rounded once and the middle is derived by subtraction, for the
same reason as chat's right pane: three independent roundings disagree with the
frame at some widths. The three boxes then render exactly `frameWidth` wide. The
middle and right boxes carry their gap as a left margin, as chat's output pane
does. Thirty, forty, thirty gives the list the widest column, because it is the
one being navigated, and the two sides the same, because neither is more
important than the other.

**The fallback.** Three boxes need room: below 76 inner columns or 10 body rows
the view reverts to exactly the layout it had before — one box, the list, and
the dispatch log below it when there is one. 76 is chosen so that an
80-column terminal still gets three panes, with sides of twenty columns and a
card text width of twenty-two. Above that threshold the three boxes are the
page's shape, not a reward for having data: while it is loading, when the load
failed, when there are no items and when the filter admits none, the sentence
saying so sits in the middle box, where the cards would be, and the two side
boxes are empty. A page that changed shape between an empty database and a full
one would look like two different pages, and the first one anyone sees is the
empty one.

**Selection.** The page opens with nothing selected: the cards are drawn, and
the side boxes are blank rather than describing whichever item happened to sort
first — which `enter` would otherwise have offered to dispatch without the user
ever having chosen it. `↑` or `↓` selects the first item, and from then on moves
the selection; `esc` returns to nothing selected, after first clearing a
finished dispatch's log if one is covering the right box. The selection is kept
by item id, not by row, across a reload, a filter change and the reload that
follows a dispatch, so a rebuilt list cannot slide a different item under the
cursor; an item the rebuild no longer shows leaves nothing selected. It
survives switching tabs, so only the first visit is empty.

**Cards.** Each item in the middle box is its own small rounded box, as wide as
the pane, under a one-line heading for its project; the list is grouped by
project exactly as the single-box list is. A card is always four rows — two
border rows around a title line and a line with the id, the age and the status —
and the fixed height is what keeps scrolling arithmetic rather than
measurement. The list scrolls by line, not by item, so that the selected card is
always whole on screen; when it is the first card in its group the heading above
it is kept in view too, because a card whose project has scrolled off has lost
its context. When a card is narrow the age gives way before the id or the status.

A card's border says what it is: the ordinary border colour normally, off-blue
when it is selected, and orange while it is waiting for the `y` or `n` that
confirms a dispatch — the same orange as a warning, because that is what the
confirmation is.

**The width rule, again, harder.** Every line put inside a pane or a card has to
be no wider than that pane, or lipgloss wraps it, the pane grows a row, and
`MaxHeight` takes that row out of the bottom border. The frame does not get any
wider or taller when that happens, which is what makes it easy to miss. Wrapping
is not enough to prevent it, because wrapping breaks on whitespace and a path or
a URL has none: every line is wrapped and then truncated. Both happen before any
styling, because truncation counts runes, and an escape sequence is runes.

**Git state is read lazily.** The left box shows the project's branch, head,
whether the tree is clean, and its latest commit. That is six `git`
subprocesses, so it is read per project, off the event loop, the first time an
item from that project is selected, and cached until `r` reloads the view or a
dispatch into that project finishes — the agent will have changed its files.
Until the read returns, the box says so. Reading the whole portfolio on every
reload would be the slowest thing the TUI does, in order to show one project.

The right box scrolls on its own with `pgup` and `pgdn` (or `ctrl+u` and
`ctrl+d`), half a pane at a time, and says there is more with a `…` on its last
row. While a dispatch runs, and until `esc` clears it afterwards, the right box
shows the dispatch log instead of the item.

## 4. The palette

Gruvbox dark, written as hex rather than 256-colour indices. lipgloss hands
every colour to termenv, which downsamples it to the nearest entry the terminal
actually advertises, so naming the exact value gets gruvbox where truecolor
exists and the closest approximation where it does not — strictly better than
picking an index that is already an approximation everywhere.

| role | colour | |
| --- | --- | --- |
| active tab | `#1d2021` on `#83a598` | dark on off-blue, bold |
| inactive tab, chrome, hints | `#928374` | grey |
| section labels | `#fabd2f` | yellow, bold |
| the user's own turns | `#83a598` | off-blue |
| warnings | `#fe8019` | orange |
| errors | `#fb4934` | red |
| done, healthy | `#b8bb26` | green |
| selected row | `#fbf1c7` on `#3c3836` | brightest on dark grey |
| the border | `#665c54` | dark grey |
| selected card border | `#83a598` | off-blue |
| card awaiting confirmation | `#fe8019` | orange |

Warnings are orange rather than gruvbox's yellow because yellow is the colour of
a section label, and a warning that looks like a heading is not doing its job.

The selected row is the one style that carries a background, which is why every
row rendered with it is padded out to the full inner width first: a highlight
that stops where the text stops reads as a rendering fault rather than as a
cursor.

## 5. Never ask the terminal a question while the program is running

Some terminal facts are not local lookups. Asking for the background colour
writes an OSC 11 query and reads the terminal's reply back off stdin, and that
makes *when* it is asked part of the contract.

Before the Bubble Tea program starts, the library doing the asking consumes its
own reply and all is well. Afterwards, Bubble Tea owns stdin: the reply is read
by its input loop, delivered as ordinary keystrokes, and typed into whatever
has focus. Apex shipped exactly that in v1.1.0 — Glamour's `WithAutoStyle`
resolves light or dark by asking, the renderer is rebuilt whenever the wrap
width changes, and the first `WindowSizeMsg` arrives immediately after start.
The TUI opened with `11;rgb:2828/2c2c/3434` already typed into the chat input,
and again on every resize.

So the Glamour style is resolved once, by name, in `New`, which runs before
`p.Run()`, and every later rebuild reuses that name. `GLAMOUR_STYLE` overrides
it, and a stdout that is not a character device resolves to `notty` without
asking anything, because a redirected TUI should not be writing escape
sequences into a file.

The general rule: anything that probes the terminal belongs before the program
starts. Nothing in the rendered output can reveal a breach of it, which is why
`TestGlamourStyleIsResolvedBeforeTheProgramStarts` counts the lookups instead.

## 6. What is checked mechanically

A layout cannot be unit-tested into being attractive, but it can be stopped from
being broken. These tests do that, and none of them should be weakened to make a
change pass:

- **`TestFrameFitsTheTerminal`** renders all three views at five sizes,
  including a deliberately cramped one, and asserts that no line is wider than
  the terminal and no frame taller. It is the only test that measures the whole
  frame rather than one view's body, and it covers both chat layouts and both
  items layouts: its widest three sizes take the panes, its narrowest two take
  the fallback. It also checks that every box opening on the body's top row
  closes on its bottom row, which is the only symptom of a line too wide for its
  pane (§3), and its items fixture carries a path longer than any pane to
  provoke one. The items view is rendered three times at each size: as it
  opens, with nothing selected; with the item selected, so that the path goes
  through the side boxes; and with no items at all.
- **`TestChatPanesSplitSpeakers`** asserts that each pane holds only its own
  speaker, and that the fallback predicate is still false at a narrow width, so
  that removing the fallback fails rather than silently producing a ten-column
  pane.
- **`TestItemsPanesFollowTheSelection`** renders the items view's side panes one
  at a time and asserts that they describe the item under the cursor — the three
  panes share every line of the frame, so asserting on the frame would find the
  wrong project's digest just as happily as the right one — that git state
  delivered while another view is focused still arrives, and that the fallback
  predicate is false at a narrow width.
- **`TestItemsStartWithNothingSelected`** asserts that the side boxes are blank
  until something is selected, that no git read starts before then, that `esc`
  empties them again, that a reload keeps the selection by id, and that an empty
  database still gets three boxes with the reason in the middle one.
- **`TestGlamourStyleIsResolvedBeforeTheProgramStarts`** counts style lookups
  and fails if a resize causes a second one, which is the only way to catch §5
  from inside a test.

Note what none of them can see. A too-narrow block still fits, so a
miscalculation that renders the panes four columns short of the frame passes all
of them
— that one was caught by rendering frames and reading them, which remains the
only way to check that the result looks like anything.
