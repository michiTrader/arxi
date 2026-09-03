package designer

import (
	"fmt"
	"strings"
)

// Point is a place on the screen, counted from the top left in rows and columns.
type Point struct {
	Row, Col int
}

// Frame is a whole screen: exactly the lines the adapter draws, and where the
// terminal's own cursor belongs.
//
// Exactly h lines, each at most w columns, is the contract and not a wish. A
// frame short of h leaves the rows below it holding whatever the last frame put
// there, and a line longer than w wraps -- which pushes every row after it down
// one and scrolls the top of the screen away. Both look like a corrupted display
// rather than a layout bug, which is why the invariant is asserted for every
// screen at several sizes instead of being left to the eye.
//
// ShowCursor is false on the screens that have no text field. A terminal cursor
// blinking in the middle of a list of files suggests you can type there, and the
// two screens where you can are the two where it is true.
type Frame struct {
	Lines      []string
	Cursor     Point
	ShowCursor bool
}

// String is the frame as a test wants to read it, and as it will look on screen.
func (f Frame) String() string { return strings.Join(f.Lines, "\n") }

// Render is the whole of what the designer looks like: a pure function from the
// model and the window size to the frame.
//
// The shape of every screen is the same three parts, because a screen whose parts
// move around is a screen the operator has to re-read: a title row that says
// which question this is, the body, and at the bottom the refusal followed by the
// keys. The keys are last because that is where the eye goes when a screen is
// unfamiliar, and the refusal is next to them because it is about the key that
// was just pressed.
//
// The body gets what is left, and the members list scrolls inside it. When even
// the fixed rows do not fit -- a three-row window, a terminal mid-resize -- the
// frame is cut to h from the bottom: the title and the body outrank the hints,
// because the hints are the part the operator can guess.
func Render(m Model, w, h int) Frame {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	head := []string{clip(m.title(), w)}
	foot := append(wrapTo(m.Err, w, 4), clip(m.hints(), w))

	avail := h - len(head) - len(foot)
	if avail < 0 {
		avail = 0
	}
	body, cur := m.body(w, avail)

	out := make([]string, 0, h)
	out = append(out, head...)
	out = append(out, body...)
	out = append(out, foot...)
	for len(out) < h {
		out = append(out, "")
	}

	f := Frame{Lines: out[:h], ShowCursor: cur.Row >= 0}
	if f.ShowCursor {
		f.Cursor = Point{Row: clampTo(len(head)+cur.Row, h-1), Col: clampTo(cur.Col, w-1)}
	}
	return f
}

func clampTo(v, max int) int {
	if v < 0 {
		return 0
	}
	if v > max {
		return max
	}
	return v
}

// title says which question this is and how many there are.
//
// A designer that shows one screen at a time owes the operator the shape of the
// whole thing: four questions, and this is the second of them. Without the count
// the only way to find out how long this takes is to finish it.
func (m Model) title() string {
	if m.Screen == ScreenDone {
		return "arxi blueprint designer -- done"
	}
	return fmt.Sprintf("arxi blueprint designer -- %s (%d of 4)", m.Screen, int(m.Screen)+1)
}

// hints are the keys THIS screen answers to, and nothing else.
//
// tab is absent from the review row because tab does not write there, and esc
// reads "quit" on the first screen and "back" on the others because that is what
// it does. A hint row that is the same on every screen is a hint row nobody reads
// twice, and one that lists a key the screen ignores is worse than blank.
func (m Model) hints() string {
	switch m.Screen {
	case ScreenName:
		return "  enter/tab continue    esc quit    ctrl+c quit"
	case ScreenMembers:
		return "  space pick    up/down move    enter/tab continue    esc back"
	case ScreenStages:
		return "  enter add    empty enter continue    backspace undo    esc back"
	case ScreenReview:
		return "  enter write the file    esc back    ctrl+c quit"
	case ScreenDone:
		return "  any key leaves"
	}
	return ""
}

// body is the middle of the screen -- the question itself -- in exactly h rows.
//
// Exactly h and padded, because the refusal and the hints sit underneath: a body
// that returned fewer rows than it was given would float them up into the middle
// of the screen on whichever screen happens to have less to say.
//
// The Point is where the terminal cursor belongs, counted from the body's first
// row, and Row -1 means this screen has no text field.
func (m Model) body(w, h int) ([]string, Point) {
	var lines []string
	cur := Point{Row: -1}
	switch m.Screen {
	case ScreenName:
		lines, cur = m.nameBody()
	case ScreenMembers:
		lines = m.membersBody(h)
	case ScreenStages:
		lines, cur = m.stagesBody()
	case ScreenReview:
		lines = m.reviewBody()
	case ScreenDone:
		lines = m.doneBody()
	}
	out := make([]string, 0, h)
	for i, l := range lines {
		if i == h {
			break
		}
		out = append(out, clip(l, w))
	}
	for len(out) < h {
		out = append(out, "")
	}
	return out, cur
}

const namePrompt = "  name  "

// nameBody is one field and what the name will be used for.
//
// It does not name the file it will become, though that is the obvious line to
// write here: the directory belongs to the store and is a flag away from being
// somewhere else, so a hardcoded `agents/release.yaml` would be a guess printed
// as a fact. The receipt on the last screen names the real path, because by then
// CreateTeam has returned one.
func (m Model) nameBody() ([]string, Point) {
	name := m.Name.String()
	lines := []string{"", namePrompt + name, ""}
	if name == "" {
		lines = append(lines, "  the blueprint's name, and what `arxi run start <name>` is given")
	} else {
		lines = append(lines,
			fmt.Sprintf("  `arxi agent show %s` will print it, `arxi run start %s` will run it", name, name))
	}
	return lines, Point{Row: 1, Col: runeLen(namePrompt) + m.Name.pos}
}

// membersBody is the store's listing with a cursor on it, windowed to the rows
// this frame has.
//
// The count row carries the scroll position -- `2 of 40 picked -- showing 12-19`
// -- instead of a scrollbar. It costs no row, it says both that there is more and
// where in it you are, and it is readable in a test's output, which a highlighted
// column down the right-hand side is not.
//
// A picked row shows its pick NUMBER and not a tick, because the order is a
// choice: it is the order the members appear in the file and the order `agent
// show` prints them, so it has to be visible before the review screen rather
// than only on it.
func (m Model) membersBody(h int) []string {
	if len(m.Cands) == 0 {
		return []string{
			"",
			"  the store holds no agents, so there is nobody to put in a team",
			"",
			"  `arxi agent create <name> --model <id> --tools read` writes one",
		}
	}
	rows := h - 2 // the count row, and the blank under it
	if rows < 1 {
		rows = 1
	}
	first := scrollTo(m.Cursor, len(m.Cands), rows)
	last := first + rows
	if last > len(m.Cands) {
		last = len(m.Cands)
	}
	out := make([]string, 0, last-first+2)
	out = append(out, m.pickCount(first, last), "")
	width := nameWidth(m.Cands)
	for i := first; i < last; i++ {
		out = append(out, m.candRow(i, width))
	}
	return out
}

func (m Model) pickCount(first, last int) string {
	s := fmt.Sprintf("  %d of %d picked", len(m.Picked), len(m.Cands))
	if last-first < len(m.Cands) {
		s += fmt.Sprintf(" -- showing %d-%d", first+1, last)
	}
	return s
}

// candRow is one file: whether it is picked, what it is called, and what picking
// it would put in the team.
//
// The right-hand column is the member's model, and it says the member's NAME
// only when that name differs from the file's. Equal is the normal case -- `agent
// create` writes them that way -- and a row reading `api  api` teaches nothing,
// while `backend  member "api"` is the one fact that explains why a later refusal
// talks about a member nobody typed.
func (m Model) candRow(i, width int) string {
	c := m.Cands[i]
	mark := "[ ]"
	if c.Unusable != "" {
		mark = "[-]"
	}
	for n, p := range m.Picked {
		if p == i {
			mark = fmt.Sprintf("[%d]", n+1)
		}
	}
	cursor := "  "
	if i == m.Cursor {
		cursor = "> "
	}
	right := c.Unusable
	if right == "" {
		right = c.Member.Model
		if c.Member.Name != c.Name {
			right = fmt.Sprintf("member %q -- %s", c.Member.Name, c.Member.Model)
		}
	}
	return cursor + pad(mark, 3) + " " + pad(c.Name, width) + "  " + right
}

// scrollTo is the first visible index: the window that keeps the cursor on
// screen, centred when there is room on both sides of it.
//
// Centred rather than the cheaper "scroll only when the cursor walks off an
// edge", because that one depends on where the window was BEFORE the keystroke.
// It would make the frame a function of history and put a scroll offset in the
// model, and the model would then have two things to keep agreeing -- for a
// difference the operator cannot see.
func scrollTo(cursor, n, rows int) int {
	if n <= rows {
		return 0
	}
	first := cursor - rows/2
	if first > n-rows {
		first = n - rows
	}
	if first < 0 {
		first = 0
	}
	return first
}

const stagePrompt = "  stage  "

// stagesBody puts the field ABOVE the list it fills.
//
// Above, because the field is where the cursor is and the list has no bound: a
// list drawn first would push the field off the bottom of a short window, and an
// operator typing at a row that is not on screen has no way to see the typo. It
// also reads in the order it happens -- what is being added, then what has been.
func (m Model) stagesBody() ([]string, Point) {
	out := []string{"", stagePrompt + m.Stage.String(), ""}
	if len(m.Stages) == 0 {
		out = append(out,
			"  no stages yet: enter on an empty line takes the default, one stage",
			"  called `work` that every member is active in")
	} else {
		out = append(out, fmt.Sprintf("  %d so far, in order:", len(m.Stages)))
		for i, s := range m.Stages {
			out = append(out, fmt.Sprintf("    %d. %s", i+1, s))
		}
	}
	return out, Point{Row: 1, Col: runeLen(stagePrompt) + m.Stage.pos}
}

// reviewBody is the file that is about to exist, described in the terms the file
// will use.
//
// It walks Picked rather than Team().Members, which is the same walk, because the
// candidate still knows which FILE each member was copied out of and the composed
// member does not. `1. api  claude-sonnet-4  (agents/backend.yaml)` is the row
// that lets an operator catch the wrong pick here, where esc still fixes it.
//
// A member's own stages are printed when it has any, because they are the half of
// the cross-check that is invisible otherwise: Validate refuses a member active
// in `build` when the team's stages are `draft, review`, and a review screen that
// showed only the team's stages would make that refusal look like a bug.
func (m Model) reviewBody() []string {
	out := []string{"", fmt.Sprintf("  %s -- %d members", m.Name.String(), len(m.Picked))}
	for n, p := range m.Picked {
		c := m.Cands[p]
		row := fmt.Sprintf("    %d. %s  %s", n+1, c.Member.Name, c.Member.Model)
		if len(c.Member.Stages) > 0 {
			row += "  active in " + strings.Join(c.Member.Stages, ", ")
		}
		out = append(out, row+"  (from "+c.Path+")")
	}
	out = append(out, "")
	if len(m.Stages) == 0 {
		out = append(out, "  stages: work -- the default, and every member is active in it")
	} else {
		out = append(out, "  stages: "+strings.Join(m.Stages, " -> "))
	}
	return append(out, "", "  enter writes it. there is no second confirmation")
}

// doneBody is a receipt, and the useful thing on a receipt is the next command.
//
// It prints the path the store returned rather than one composed from the name,
// so what is on screen is where the file actually is.
func (m Model) doneBody() []string {
	if m.Wrote == "" {
		return []string{"", "  nothing was written"}
	}
	name := m.Name.String()
	return []string{
		"",
		"  wrote " + m.Wrote,
		"",
		fmt.Sprintf("  `arxi agent show %s` prints what is in it", name),
		fmt.Sprintf("  `arxi run start %s --task '...'` starts a run of it", name),
	}
}

func runeLen(s string) int { return len([]rune(s)) }

// clip cuts a line to w columns, counting runes and never bytes.
//
// One column per rune is the assumption, and it is wrong for a double-width
// glyph: a CJK member name will push its row one column over per character. The
// alternative is a width table this package would have to carry and keep current,
// and the failure it would prevent is a cosmetic column -- whereas the failure of
// cutting bytes instead of runes is half a rune and a replacement glyph on screen.
func clip(s string, w int) string {
	if w < 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return string(r[:w])
}

// pad right-fills to w so that a column of names lines up. A string already at
// least w wide is returned as it is: one long file name shifts its own row rather
// than widening every other one.
func pad(s string, w int) string {
	if n := w - runeLen(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

func nameWidth(cs []Candidate) int {
	w := 0
	for _, c := range cs {
		if n := runeLen(c.Name); n > w {
			w = n
		}
	}
	if w > 24 {
		w = 24
	}
	return w
}

// wrapTo turns a refusal into at most max rows of at most w columns.
//
// Wrapped and not cut, because the designer's refusals are sentences and the
// point of them is the second half: `agents/api.yaml and agents/legacy-api.yaml
// would both call it "api"` names the two files in the first eighty columns and
// says why one team cannot hold both in the rest. A refusal cut at the window's
// edge is a refusal that has to be reproduced somewhere wider to be understood.
//
// The cap exists because a refusal that fills the screen hides the thing it is
// about; past it the text ends in an ellipsis, which is at least honest about
// there being more.
func wrapTo(s string, w, max int) []string {
	if s == "" || max < 1 {
		return nil
	}
	out := wrapWords(s, w)
	if len(out) <= max {
		return out
	}
	out = out[:max]
	if w >= 4 {
		out[max-1] = clip(out[max-1], w-3) + "..."
	}
	return out
}

// wrapWords is a greedy fill, marking the first row so the block is findable
// without colour and indenting the rest so it reads as one thing.
//
// strings.Fields splits on newlines as well as spaces, so a message that reached
// here without passing through oneLine still cannot break the frame.
func wrapWords(s string, w int) []string {
	const lead, cont = "  ! ", "    "
	var out []string
	prefix, line := lead, lead
	for _, word := range strings.Fields(s) {
		switch {
		case line == prefix:
			line += word
		case runeLen(line)+1+runeLen(word) <= w:
			line += " " + word
		default:
			out = append(out, clip(line, w))
			prefix, line = cont, cont+word
		}
	}
	return append(out, clip(line, w))
}
