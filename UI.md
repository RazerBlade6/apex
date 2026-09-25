# Apex UI

The visual contract for the terminal interface: the frame and its arithmetic,
the layout of the chat view, and the palette.

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

## 3. The palette

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

Warnings are orange rather than gruvbox's yellow because yellow is the colour of
a section label, and a warning that looks like a heading is not doing its job.

The selected row is the one style that carries a background, which is why every
row rendered with it is padded out to the full inner width first: a highlight
that stops where the text stops reads as a rendering fault rather than as a
cursor.

## 4. What is checked mechanically

A layout cannot be unit-tested into being attractive, but it can be stopped from
being broken. Two tests do that, and neither should be weakened to make a change
pass:

- **`TestFrameFitsTheTerminal`** renders all three views at five sizes,
  including a deliberately cramped one, and asserts that no line is wider than
  the terminal and no frame taller. It is the only test that measures the whole
  frame rather than one view's body, and it covers both chat layouts: its widest
  three sizes take the two panes, its narrowest two take the fallback.
- **`TestChatPanesSplitSpeakers`** asserts that each pane holds only its own
  speaker, and that the fallback predicate is still false at a narrow width, so
  that removing the fallback fails rather than silently producing a ten-column
  pane.

Note what neither of them can see. A too-narrow block still fits, so a
miscalculation that renders the pair four columns short of the frame passes both
— that one was caught by rendering frames and reading them, which remains the
only way to check that the result looks like anything.
