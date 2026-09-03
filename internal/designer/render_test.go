package designer

import (
	"fmt"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/agentstore"
	"github.com/michiTrader/arxi/internal/kernel"
)

// named is one model with a word for which screen it is, so that an invariant can
// be asserted about ALL of them instead of about the one that was easiest to
// build. Every screen is reachable by keys, so every fixture here is built by
// pressing them.
type named struct {
	what string
	m    Model
}

func screens(t *testing.T) []named {
	t.Helper()
	typing, _ := fold(picker(), typeIn("release")...)
	taken, _ := fold(picker(), seq(typeIn("platform"), []Key{Press(KeyEnter)})...)
	picked, _ := fold(onMembers(t), Press(KeyDown), Typed(' '), Press(KeyUp), Typed(' '))
	refused, _ := fold(onMembers(t), seq([]Key{Typed(' ')}, down(2), []Key{Typed(' ')})...)
	stages, _ := fold(onStages(t), seq(typeIn("draft"), []Key{Press(KeyEnter)})...)
	done, _ := Update(onReview(t), Written{Path: "agents/release.yaml"})

	empty := New(nil)
	empty.Screen = ScreenMembers

	return []named{
		{"name, half typed", typing},
		{"name, refused", taken},
		{"members, two picked", picked},
		{"members, a refusal on screen", refused},
		{"members, an empty store", empty},
		{"stages, one added", stages},
		{"review", onReview(t)},
		{"done", done},
	}
}

// candRowFor is the listing row for one file.
//
// Found by the file name after the pick marker rather than by a bare substring,
// because a substring finds the wrong row: `api` is inside `legacy-api` and
// inside platform's `is itself a team of 2 (api, web)`.
func candRowFor(t *testing.T, f Frame, name string) string {
	t.Helper()
	for _, l := range f.Lines {
		if strings.Contains(l, "] "+name+" ") {
			return l
		}
	}
	t.Fatalf("no row for %q in:\n%s", name, f)
	return ""
}

// TestEveryScreenFillsTheWindowExactlyAtEverySize is the frame contract, and it
// is one test over every screen and nine sizes because the sizes that break a
// layout are never the size it was written at.
//
// A frame short of h leaves the rows below it showing the previous frame, so a
// refusal stays on screen after the screen it belonged to is gone. A line longer
// than w wraps, which pushes every row after it down one and scrolls the title
// off the top. Neither looks like a layout bug from the operator's chair -- both
// look like the tool is broken -- and both are one arithmetic slip away at all
// times.
//
// The escape check is here rather than in its own test because it is the same
// contract: cmd/arxi owns every byte that moves a cursor or sets a colour. An
// escape sequence built into a frame line would be a sequence this package cannot
// see the effect of, and it would arrive in `%q` in some other test's output.
func TestEveryScreenFillsTheWindowExactlyAtEverySize(t *testing.T) {
	sizes := []struct{ w, h int }{
		{80, 24}, {120, 40}, {80, 6}, {40, 24}, {20, 8}, {200, 2}, {10, 3}, {1, 1}, {0, 0},
	}
	for _, s := range screens(t) {
		for _, sz := range sizes {
			w, h := sz.w, sz.h
			if w < 1 {
				w = 1
			}
			if h < 1 {
				h = 1
			}
			f := Render(s.m, sz.w, sz.h)
			if len(f.Lines) != h {
				t.Errorf("%s at %dx%d drew %d lines, not %d.\n%s",
					s.what, sz.w, sz.h, len(f.Lines), h, f)
				continue
			}
			for i, l := range f.Lines {
				if n := runeLen(l); n > w {
					t.Errorf("%s at %dx%d: line %d is %d columns wide and will wrap: %q",
						s.what, sz.w, sz.h, i, n, l)
				}
				if strings.ContainsAny(l, "\n\r") {
					t.Errorf("%s at %dx%d: line %d holds a newline, which shifts every row "+
						"under it up: %q", s.what, sz.w, sz.h, i, l)
				}
				if strings.ContainsRune(l, 0x1b) {
					t.Errorf("%s at %dx%d: line %d carries an escape sequence; the terminal "+
						"belongs to cmd/arxi: %q", s.what, sz.w, sz.h, i, l)
				}
			}
			if f.ShowCursor && (f.Cursor.Row < 0 || f.Cursor.Row >= h ||
				f.Cursor.Col < 0 || f.Cursor.Col >= w) {
				t.Errorf("%s at %dx%d puts the cursor at %+v, which is off the window",
					s.what, sz.w, sz.h, f.Cursor)
			}
		}
	}
}

// TestTheCursorSitsOnTheRuneItIsBeforeAndNotOnTheByte.
//
// The field counts runes and the frame has to agree with it, or the caret on
// screen is not where the next character will go -- which is the one thing a text
// box has to get right.
func TestTheCursorSitsOnTheRuneItIsBeforeAndNotOnTheByte(t *testing.T) {
	m, _ := fold(picker(), typeIn("release")...)
	f := Render(m, 80, 24)
	if !f.ShowCursor {
		t.Fatal("the name screen is a text field, and the frame hides the cursor on it")
	}
	line := f.Lines[f.Cursor.Row]
	if !strings.Contains(line, "release") {
		t.Fatalf("the cursor is on row %d, which is %q and not the name being typed",
			f.Cursor.Row, line)
	}
	if want := runeLen(namePrompt) + len("release"); f.Cursor.Col != want {
		t.Errorf("the cursor is at column %d and the text ends at column %d",
			f.Cursor.Col, want)
	}

	// Two lefts move the cursor and nothing else. A text box that redrew its text
	// as the cursor walked through it would be unreadable at the one moment it is
	// being read: while a typo in the middle is being found.
	back, _ := fold(m, Press(KeyLeft), Press(KeyLeft))
	g := Render(back, 80, 24)
	if g.Cursor.Col != f.Cursor.Col-2 {
		t.Errorf("two left arrows moved the cursor to column %d, from %d",
			g.Cursor.Col, f.Cursor.Col)
	}
	if got := g.Lines[g.Cursor.Row]; got != line {
		t.Errorf("moving the cursor rewrote the row:\n  was %q\n  now %q", line, got)
	}

	// An accent is two bytes and one column. A column counted in bytes puts the
	// caret one place past the end of a name like this one, and every rune typed
	// after that is drawn where it is not.
	acc, _ := fold(picker(), typeIn("café")...)
	k := Render(acc, 80, 24)
	if want := runeLen(namePrompt) + 4; k.Cursor.Col != want {
		t.Errorf("after typing a four-rune five-byte name the cursor is at column %d, "+
			"and the text ends at column %d", k.Cursor.Col, want)
	}
	if got := runeLen(k.Lines[k.Cursor.Row]); got != k.Cursor.Col {
		t.Errorf("the row is %d columns wide and the cursor is at column %d; it belongs "+
			"just after the last rune", got, k.Cursor.Col)
	}
}

// TestOnlyAScreenYouCanTypeOnShowsACursor: a terminal cursor blinking in the
// middle of a list of files says you can type there, and on three of the five
// screens you cannot.
func TestOnlyAScreenYouCanTypeOnShowsACursor(t *testing.T) {
	for _, s := range screens(t) {
		want := strings.HasPrefix(s.what, "name") || strings.HasPrefix(s.what, "stages")
		if got := Render(s.m, 80, 24).ShowCursor; got != want {
			t.Errorf("on the %s screen ShowCursor is %v, want %v", s.what, got, want)
		}
	}
}

// TestThePickerShowsThePickOrderAndWhyAFileCannotBePicked.
//
// This screen is most of what the designer is for: the names are in front of you,
// and so is the reason two of them cannot be used. A row that carried only `[-]`
// would send the operator to the file to work out which of the three reasons it
// is, which is the trip this screen exists to save.
func TestThePickerShowsThePickOrderAndWhyAFileCannotBePicked(t *testing.T) {
	m, _ := fold(onMembers(t), Press(KeyDown), Typed(' '), Press(KeyUp), Typed(' '))
	f := Render(m, 100, 24)

	if want := "2 of 5 picked"; !strings.Contains(f.String(), want) {
		t.Errorf("the count row never says %q:\n%s", want, f)
	}
	// reviewer was picked first and api second, which is the order they will be
	// written in, so it is the order the rows have to show.
	for name, want := range map[string]string{"reviewer": "[1]", "api": "[2]"} {
		if row := candRowFor(t, f, name); !strings.Contains(row, want) {
			t.Errorf("%s is picked and its row does not carry its place in the order (%s): %q",
				name, want, row)
		}
	}
	if row := candRowFor(t, f, "api"); !strings.HasPrefix(row, "> ") {
		t.Errorf("the cursor is on api and its row is not the one marked: %q", row)
	}
	for _, tc := range []struct{ file, want string }{
		{"platform", "[-]"},
		{"platform", "is itself a team of 2"},
		{"platform", "api, web"},
		{"half-written", "did not load"},
		{"half-written", "model is required"},
	} {
		if row := candRowFor(t, f, tc.file); !strings.Contains(row, tc.want) {
			t.Errorf("%s cannot be a member and its row does not say %q: %q",
				tc.file, tc.want, row)
		}
	}
	// The member name is shown when it differs from the file name and left out
	// when it does not: `api  api` teaches nothing, and `legacy-api  member "api"`
	// is the fact that explains a refusal about a member nobody typed.
	if row := candRowFor(t, f, "legacy-api"); !strings.Contains(row, `member "api"`) {
		t.Errorf("legacy-api carries a member called api and its row hides it: %q", row)
	}
	if row := candRowFor(t, f, "api"); !strings.Contains(row, "claude-sonnet-4") ||
		strings.Contains(row, "member") {
		t.Errorf("api's row should be the model alone, since the two names agree: %q", row)
	}
}

// TestALongListScrollsToKeepTheCursorOnScreenAndSaysWhereItIs.
//
// Forty agents in a twenty-row window is the case the package doc names, and it is
// the case a designer written against a directory of three never meets. The cursor
// has to be on screen at every position in the list, including both ends, and the
// operator has to be able to tell that there is more of the list than this.
func TestALongListScrollsToKeepTheCursorOnScreenAndSaysWhereItIs(t *testing.T) {
	es := make([]agentstore.Entry, 0, 40)
	for i := 0; i < 40; i++ {
		name := fmt.Sprintf("agent-%02d", i)
		es = append(es, stored(name, member(name)))
	}
	m := New(Candidates(es))
	m.Screen = ScreenMembers

	for _, cursor := range []int{0, 1, 19, 20, 38, 39} {
		m.Cursor = cursor
		row := candRowFor(t, Render(m, 80, 20), fmt.Sprintf("agent-%02d", cursor))
		if !strings.HasPrefix(row, "> ") {
			t.Errorf("with the cursor on row %d that row is drawn unmarked, so some other "+
				"row looks like the one space would pick: %q", cursor, row)
		}
	}

	m.Cursor = 39
	f := Render(m, 80, 20)
	if want := "showing 25-40"; !strings.Contains(f.String(), want) {
		t.Errorf("at the bottom of forty rows in a twenty-row window nothing says %q, so\n"+
			"  the operator cannot tell this is a window onto a longer list:\n%s", want, f)
	}
	// A list that fits says nothing about a window, because there is nothing to say.
	if got := Render(m, 80, 60).String(); strings.Contains(got, "showing") {
		t.Errorf("all forty rows fit and the count row still talks about a window:\n%s", got)
	}
}

// TestALongRefusalIsWrappedWholeAndCannotEatTheScreen.
//
// The designer's refusals are sentences and the second half is the half that says
// what to do: the duplicate-member refusal names two files in its first sixty
// columns and says why one team cannot hold both in the rest. A refusal cut at the
// edge of the window would have to be reproduced in a wider terminal to be
// understood, which is the opposite of the point of refusing early.
//
// It is also bounded, because a refusal that filled the screen would hide the
// thing it is about -- and the keys must stay on the bottom row whatever is being
// said above them.
func TestALongRefusalIsWrappedWholeAndCannotEatTheScreen(t *testing.T) {
	m, _ := fold(onMembers(t), seq([]Key{Typed(' ')}, down(2), []Key{Typed(' ')})...)
	if m.Err == "" {
		t.Fatal("picking two files that would carry one member name was not refused")
	}
	f := Render(m, 60, 24)
	if !strings.Contains(f.String(), "  ! ") {
		t.Errorf("the refusal is not marked, so it reads as one more body row:\n%s", f)
	}
	flat := strings.Join(strings.Fields(f.String()), " ")
	if want := strings.Join(strings.Fields(m.Err), " "); !strings.Contains(flat, want) {
		t.Errorf("the refusal on screen is not the whole refusal:\n  want %q\n  got\n%s", want, f)
	}

	m.Err = strings.TrimSpace(strings.Repeat("a refusal that will not stop talking ", 40))
	g := Render(m, 60, 24)
	if !strings.Contains(g.String(), "...") {
		t.Errorf("a refusal of two hundred words was neither bounded nor marked as cut:\n%s", g)
	}
	if last := g.Lines[len(g.Lines)-1]; !strings.Contains(last, "space pick") {
		t.Errorf("a long refusal pushed the keys off the bottom row, which now reads %q", last)
	}
}

// TestReviewNamesTheFileEachMemberCameFromAndTheStagesItWants.
//
// Two facts on this screen are ones the composed team no longer holds: which file
// each member was copied out of, and that a hand-edited file's member is called
// something other than the file. Both are what makes a wrong pick catchable here,
// where esc still undoes it.
//
// A member's own stages are printed for the same reason. Validate refuses a member
// active in `build` when the team's stages are `draft, review`, and a review screen
// that showed only the team's stages would make that refusal look like a bug.
func TestReviewNamesTheFileEachMemberCameFromAndTheStagesItWants(t *testing.T) {
	m := New(Candidates([]agentstore.Entry{
		stored("backend", kernel.MemberConfig{
			Name: "api", Model: "claude-sonnet-4", Stages: []string{"build"},
		}),
	}))
	m.Name = newField("release")
	m.Picked = []int{0}
	m.Stages = []string{"build", "review"}
	m.Screen = ScreenReview

	got := Render(m, 100, 24).String()
	for _, want := range []string{
		"release", "api", "claude-sonnet-4", "active in build",
		"agents/backend.yaml", "build -> review",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the review screen never says %q:\n%s", want, got)
		}
	}
}

// TestReviewSaysWhatNoStagesBecomesAndTheReceiptNamesTheRealPath.
//
// An empty stage list is legal and does not stay empty: the store writes one stage
// called `work`. A review screen that showed `stages:` with nothing after it would
// be describing a file that is not the file about to be written.
//
// The receipt prints the path CreateTeam returned rather than one composed out of
// the name, because the directory belongs to the store and is one flag away from
// being somewhere else. A composed path would be a guess printed as a fact, and it
// would be wrong exactly when the operator most needs to find the file.
func TestReviewSaysWhatNoStagesBecomesAndTheReceiptNamesTheRealPath(t *testing.T) {
	m := onReview(t)
	m.Stages = nil
	if got := Render(m, 80, 24).String(); !strings.Contains(got, "work") {
		t.Errorf("a team with no stages is written with one called work, and review does "+
			"not say so:\n%s", got)
	}

	done, _ := Update(onReview(t), Written{Path: "elsewhere/release.yaml"})
	got := Render(done, 80, 24).String()
	for _, want := range []string{
		"wrote elsewhere/release.yaml", "arxi agent show release", "arxi run start release",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the receipt never says %q:\n%s", want, got)
		}
	}
}

// TestTheKeysOnTheBottomRowAreTheKeysThatScreenAnswersTo.
//
// The keys are on the last row because that is where the eye goes on a screen it
// does not know, and they differ per screen because a row that is identical
// everywhere is a row nobody reads twice.
//
// Review is the case that matters. tab has meant "the next question" on the three
// screens before it and here it does nothing, so a hint row that offered tab would
// be advertising the key that writes a file by muscle memory.
func TestTheKeysOnTheBottomRowAreTheKeysThatScreenAnswersTo(t *testing.T) {
	for _, tc := range []struct {
		m      Model
		want   []string
		absent string
	}{
		{picker(), []string{"enter/tab", "esc quit"}, ""},
		{onMembers(t), []string{"space pick", "up/down"}, ""},
		{onStages(t), []string{"enter add", "backspace"}, ""},
		{onReview(t), []string{"enter write"}, "tab"},
	} {
		f := Render(tc.m, 80, 24)
		last := f.Lines[len(f.Lines)-1]
		for _, want := range tc.want {
			if !strings.Contains(last, want) {
				t.Errorf("the %s screen's bottom row is %q, which does not offer %q",
					tc.m.Screen, last, want)
			}
		}
		if tc.absent != "" && strings.Contains(last, tc.absent) {
			t.Errorf("the %s screen offers %q and does not answer to it: %q",
				tc.m.Screen, tc.absent, last)
		}
	}
}

// TestAnEmptyStoreSaysSoAndSaysWhatWritesAnAgent.
//
// A fresh installation reaches this screen with nothing on it, and an empty list is
// indistinguishable from a broken one. So the row says what the state is, and the
// row under it is the command that changes the state.
func TestAnEmptyStoreSaysSoAndSaysWhatWritesAnAgent(t *testing.T) {
	m := New(nil)
	m.Screen = ScreenMembers
	got := Render(m, 80, 24).String()
	for _, want := range []string{"no agents", "arxi agent create"} {
		if !strings.Contains(got, want) {
			t.Errorf("the picker on an empty store never says %q:\n%s", want, got)
		}
	}
}
