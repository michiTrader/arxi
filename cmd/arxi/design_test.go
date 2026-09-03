// design_test.go is about the two halves of design.go that are not the designer:
// the bytes a terminal sends, and the loop that turns them into frames.
//
// internal/designer has its own tests for what a key MEANS -- nothing here checks
// that space picks an agent or that enter advances a screen. These check the
// things only the adapter can get wrong: that the sequence a real terminal sends
// for a key arrives as that key, that a sequence for a key the designer has no
// answer for is swallowed whole instead of being typed into a name field, that
// one frame is one write of exactly the window, and that a store's refusal comes
// back to the screen that can fix it.
package main

import (
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/michiTrader/arxi/internal/agentstore"
	"github.com/michiTrader/arxi/internal/blueprint"
	"github.com/michiTrader/arxi/internal/designer"
	"github.com/michiTrader/arxi/internal/kernel"
)

// storedAgent is one usable file in the store, as List would report it.
func storedAgent(name, model string) agentstore.Entry {
	return agentstore.Entry{
		Name: name,
		Path: "agents/" + name + ".yaml",
		Blueprint: &blueprint.Blueprint{
			Name:   name,
			Config: kernel.Config{Members: []kernel.MemberConfig{{Name: name, Model: model}}},
		},
	}
}

// typing is what an operator's fingers produce for a word.
func typing(s string) []designer.Key {
	var ks []designer.Key
	for _, r := range s {
		ks = append(ks, designer.Typed(r))
	}
	return ks
}

// drive runs the real loop over a fixed script of keys.
//
// The channel is buffered and closed, which stands in for the two ways a session
// ends: the script may quit the designer, or it may run out -- and a closed keys
// channel is exactly what a terminal at EOF gives the loop. Either way run
// returns, so a test that scripts the wrong keys fails instead of hanging.
func drive(t *testing.T, w io.Writer, write func(agentstore.Team) (string, error), m designer.Model, keys ...designer.Key) designer.Model {
	t.Helper()
	ch := make(chan designer.Key, len(keys))
	for _, k := range keys {
		ch <- k
	}
	close(ch)
	return session{
		keys:  ch,
		out:   w,
		size:  func() (int, int) { return 80, 24 },
		write: write,
	}.run(m)
}

// writes keeps every Write separately, and implements nothing else: io.WriteString
// would find a WriteString method and use that, and then one frame could arrive
// as several calls with nobody able to tell.
type writes struct{ parts []string }

func (w *writes) Write(p []byte) (int, error) {
	w.parts = append(w.parts, string(p))
	return len(p), nil
}

func (w *writes) String() string { return strings.Join(w.parts, "") }

// chunks hands out one part per Read, the way a terminal hands out whatever has
// arrived so far, and then reports EOF.
type chunks struct{ parts []string }

func (c *chunks) Read(p []byte) (int, error) {
	if len(c.parts) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.parts[0])
	c.parts = c.parts[1:]
	return n, nil
}

// silence is a terminal with nothing more to say: it hands out its parts and then
// blocks, which is what stdin does between keystrokes. A reader that returned EOF
// instead would let a decoder that waits for one more byte pass anyway.
type silence struct {
	parts []string
	done  chan struct{}
}

func (s *silence) Read(p []byte) (int, error) {
	if len(s.parts) == 0 {
		<-s.done
		return 0, io.EOF
	}
	n := copy(p, s.parts[0])
	s.parts = s.parts[1:]
	return n, nil
}

// TestEveryKeyTheDesignerAnswersToArrivesFromSomeRealTerminal is the table that
// makes the designer reachable.
//
// Update answers fourteen key codes; a code no byte sequence produces is a
// screen with a key on the hints line that does nothing. The two columns per key
// are deliberate: xterm sends CSI for the arrows and DEC application mode sends
// SS3, both from the same keyboard, depending on what the program before this one
// left the terminal in. used is asserted as well as the key, because a decoder
// that returns the right key and the wrong length types the difference.
func TestEveryKeyTheDesignerAnswersToArrivesFromSomeRealTerminal(t *testing.T) {
	press := designer.Press
	for _, tc := range []struct {
		what string
		in   string
		want designer.Key
		used int
	}{
		{"return", "\r", press(designer.KeyEnter), 1},
		{"a newline, from a script or a stubborn terminal", "\n", press(designer.KeyEnter), 1},
		{"tab", "\t", press(designer.KeyTab), 1},
		{"shift+tab", "\x1b[Z", press(designer.KeyBackTab), 3},
		{"ctrl+c", "\x03", press(designer.KeyInterrupt), 1},
		{"escape, pressed twice", "\x1b\x1b", press(designer.KeyEsc), 1},
		{"backspace", "\x7f", press(designer.KeyBackspace), 1},
		{"backspace, the other byte for it", "\x08", press(designer.KeyBackspace), 1},
		{"delete", "\x1b[3~", press(designer.KeyDelete), 4},
		{"ctrl+delete, modifiers and all", "\x1b[3;5~", press(designer.KeyDelete), 6},
		{"up", "\x1b[A", press(designer.KeyUp), 3},
		{"up in application mode", "\x1bOA", press(designer.KeyUp), 3},
		{"ctrl+up", "\x1b[1;5A", press(designer.KeyUp), 6},
		{"down", "\x1b[B", press(designer.KeyDown), 3},
		{"right", "\x1b[C", press(designer.KeyRight), 3},
		{"left", "\x1b[D", press(designer.KeyLeft), 3},
		{"home", "\x1b[H", press(designer.KeyHome), 3},
		{"home, as a numbered sequence", "\x1b[1~", press(designer.KeyHome), 4},
		{"home, as the other numbered sequence", "\x1b[7~", press(designer.KeyHome), 4},
		{"end", "\x1b[F", press(designer.KeyEnd), 3},
		{"end, as a numbered sequence", "\x1b[4~", press(designer.KeyEnd), 4},
		{"end, as the other numbered sequence", "\x1b[8~", press(designer.KeyEnd), 4},
		{"end in application mode", "\x1bOF", press(designer.KeyEnd), 3},
		{"a letter", "a", designer.Typed('a'), 1},
		{"a space, which is how an agent gets picked", " ", designer.Typed(' '), 1},
		{"a letter that takes two bytes", "é", designer.Typed('é'), 2},
		{"a rune from outside the basic plane", "🙂", designer.Typed('🙂'), 4},
	} {
		t.Run(tc.what, func(t *testing.T) {
			got, used, ok := decode([]byte(tc.in))
			if !ok {
				t.Fatalf("decode(%q) produced no key: %s reaches no screen", tc.in, tc.what)
			}
			if got != tc.want {
				t.Errorf("decode(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
			if used != tc.used {
				t.Errorf("decode(%q) consumed %d bytes, want %d: the rest is typed", tc.in, used, tc.used)
			}
		})
	}
}

// TestASequenceWithNoMeaningIsSwallowedWholeAndNeverTyped is the other half of
// the decoder, and the half a name field notices.
//
// A terminal sends far more than the designer has keys for: a mouse report on
// every click if something turned tracking on, a bracketed-paste marker around
// pasted text, a device reply to a query the shell made. None of it means
// anything here. What matters is not that it produces no key but that it
// consumes every byte it came with -- a decoder that skips the escape and stops
// leaves `[<0;12;34M` to be typed into the team's name, one character at a time.
func TestASequenceWithNoMeaningIsSwallowedWholeAndNeverTyped(t *testing.T) {
	for _, tc := range []struct{ what, in string }{
		{"a mouse click", "\x1b[<0;12;34M"},
		{"a mouse release", "\x1b[<0;12;34m"},
		{"f5", "\x1b[15~"},
		{"f1, in application mode", "\x1bOP"},
		{"the start of a paste", "\x1b[200~"},
		{"the end of a paste", "\x1b[201~"},
		{"a terminal answering what it is", "\x1b[?1;2c"},
		{"a cursor position report", "\x1b[24;80R"},
		{"alt+x", "\x1bx"},
		{"ctrl+g", "\x07"},
		{"a stray nul", "\x00"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			k, used, ok := decode([]byte(tc.in))
			if ok {
				t.Errorf("decode(%q) = %+v: %s is not a key the designer has", tc.in, k, tc.what)
			}
			if used != len(tc.in) {
				t.Errorf("decode(%q) consumed %d of %d bytes: %q would be typed into the field",
					tc.in, used, len(tc.in), tc.in[used:])
			}
		})
	}
}

// TestAnIncompleteSequenceWaitsForTheRestInsteadOfBeingTyped is the case a read
// boundary creates: 256 bytes is plenty for any sequence, but nothing says the
// sequence is not split across two reads.
//
// used == 0 is how the decoder asks for more. Anything else here would be a
// half-sequence turned into characters and a tail turned into more of them.
func TestAnIncompleteSequenceWaitsForTheRestInsteadOfBeingTyped(t *testing.T) {
	for _, tc := range []struct{ what, in string }{
		{"an escape and nothing yet", "\x1b"},
		{"a csi with no final byte", "\x1b["},
		{"a csi cut off mid-parameter", "\x1b[1;5"},
		{"an ss3 with no key on it", "\x1bO"},
		{"the first byte of a two-byte rune", "\xc3"},
		{"three of the four bytes of an emoji", "\xf0\x9f\x99"},
		{"an escape and half an alt+rune", "\x1b\xc3"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			k, used, ok := decode([]byte(tc.in))
			if ok || used != 0 {
				t.Errorf("decode(%q) = %+v, %d, %v; want a request for more bytes (0, false)",
					tc.in, k, used, ok)
			}
		})
	}
}

// TestASequenceSplitBetweenTwoReadsIsStillOneKey is what the leftover buffer in
// keyStream is for.
//
// A terminal is a stream, not a message queue. The kernel returns what has
// arrived, and an arrow key pressed while the pipe was busy can arrive as `\x1b[`
// and then `A`. Without the leftover, that is an escape and a bracket and a
// capital A typed into whatever field has the cursor.
func TestASequenceSplitBetweenTwoReadsIsStillOneKey(t *testing.T) {
	for _, tc := range []struct {
		what  string
		parts []string
		want  []designer.Key
	}{
		{"a csi cut after the bracket", []string{"\x1b[", "A"}, []designer.Key{designer.Press(designer.KeyUp)}},
		{"a csi cut mid-parameter", []string{"\x1b[1;", "5B"}, []designer.Key{designer.Press(designer.KeyDown)}},
		{"a rune cut in half", []string{"\xc3", "\xa9"}, []designer.Key{designer.Typed('é')}},
		{"several keys in one read", []string{"ab\r"}, append(typing("ab"), designer.Press(designer.KeyEnter))},
		{"a key either side of a boundary", []string{"a\x1b[", "Cb"}, []designer.Key{
			designer.Typed('a'), designer.Press(designer.KeyRight), designer.Typed('b'),
		}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			var got []designer.Key
			for k := range keyStream(&chunks{parts: tc.parts}) {
				got = append(got, k)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("%q produced %d keys %+v, want %d %+v", tc.parts, len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("key %d of %q = %+v, want %+v", i, tc.parts, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestALoneEscapeArrivesWithoutWaitingForAnotherKey is the one place the decoder
// guesses, and this is the guess written down.
//
// An escape byte on its own is either the esc key or the start of a sequence
// whose rest has not arrived. Nothing in the bytes can tell the two apart; the
// only real distinguisher is a timer, and a timer would mean the loop wakes up
// when nobody has pressed anything. So a read that ends on a bare escape is taken
// as the esc key -- which is what makes it possible to leave the first screen at
// all. The cost is a sequence split exactly after the escape, which arrives as
// esc and then its own tail; the reader here blocks forever after the escape,
// because a reader at EOF would let a decoder that waited for the next read pass.
func TestALoneEscapeArrivesWithoutWaitingForAnotherKey(t *testing.T) {
	r := &silence{parts: []string{"\x1b"}, done: make(chan struct{})}
	t.Cleanup(func() { close(r.done) })

	select {
	case k, ok := <-keyStream(r):
		if !ok {
			t.Fatal("the key stream closed on a lone escape; esc is the only way off the first screen")
		}
		if want := designer.Press(designer.KeyEsc); k != want {
			t.Errorf("a lone escape arrived as %+v, want %+v", k, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a lone escape produced no key: the designer waits for a byte the operator has no reason to send")
	}
}

// TestTheDesignerWritesTheTeamItWasDrivenToAndReportsWhereItWent drives the whole
// loop with the keys an operator would press, and is the test that says the four
// screens add up to a file.
//
// The store is a function here rather than a directory, because what is under
// test is the wiring: that exactly one Write command comes out of the review
// screen, that its Team is the one the keystrokes built, and that the path the
// store answers with comes back through Update and onto the last frame. Whether
// CreateTeam writes valid YAML is agentstore's own test.
func TestTheDesignerWritesTheTeamItWasDrivenToAndReportsWhereItWent(t *testing.T) {
	m := designer.New(designer.Candidates([]agentstore.Entry{
		storedAgent("planner", "test-model"),
		storedAgent("reviewer", "test-model"),
	}))

	var got []agentstore.Team
	write := func(team agentstore.Team) (string, error) {
		got = append(got, team)
		return "agents/release.yaml", nil
	}

	enter := designer.Press(designer.KeyEnter)
	script := typing("release")
	script = append(script, enter)                               // the name is answered
	script = append(script, designer.Typed(' '), enter)          // planner is picked
	script = append(script, typing("draft")...)                  //
	script = append(script, enter, enter)                        // one stage, then no more
	script = append(script, enter)                               // review writes
	script = append(script, designer.Press(designer.KeyBackTab)) // the receipt takes any key

	var out writes
	m = drive(t, &out, write, m, script...)

	if len(got) != 1 {
		t.Fatalf("the store was asked to write %d times, want exactly 1: %+v", len(got), got)
	}
	team := got[0]
	if team.Name != "release" {
		t.Errorf("wrote a team called %q, want %q", team.Name, "release")
	}
	if len(team.Members) != 1 || team.Members[0].Name != "planner" {
		t.Errorf("wrote members %+v, want just planner", team.Members)
	}
	if len(team.Stages) != 1 || team.Stages[0] != "draft" {
		t.Errorf("wrote stages %+v, want [draft]", team.Stages)
	}
	if m.Wrote != "agents/release.yaml" {
		t.Errorf("the model ended with Wrote = %q, want the path the store answered with", m.Wrote)
	}
	if m.Screen != designer.ScreenDone || !m.Quit || m.Err != "" {
		t.Errorf("ended on screen %v, quit %v, err %q; want the receipt, left, and nothing wrong",
			m.Screen, m.Quit, m.Err)
	}
	if !strings.Contains(out.String(), "agents/release.yaml") {
		t.Error("the path never reached a frame: the designer wrote a file and did not say where")
	}
}

// TestAStoreThatRefusesTheNameSendsTheDesignerBackToTheScreenThatCanFixIt is the
// failing write, and the reason Write is a command with an answer rather than a
// call the loop makes and forgets.
//
// The candidates are read once, before raw mode, so a name that was free when the
// listing was taken can be taken by the time enter is pressed on the review
// screen. The designer cannot prevent that; what it can do is not lose the work.
// Everything picked and typed is still in the model, and the cursor is back on the
// one field that has to change.
func TestAStoreThatRefusesTheNameSendsTheDesignerBackToTheScreenThatCanFixIt(t *testing.T) {
	m := designer.New(designer.Candidates([]agentstore.Entry{storedAgent("planner", "test-model")}))

	calls := 0
	write := func(agentstore.Team) (string, error) {
		calls++
		return "", agentstore.ErrExists
	}

	enter := designer.Press(designer.KeyEnter)
	script := typing("release")
	script = append(script, enter, designer.Typed(' '), enter, enter, enter)

	var out writes
	m = drive(t, &out, write, m, script...)

	if calls != 1 {
		t.Fatalf("the store was asked to write %d times, want 1", calls)
	}
	if m.Screen != designer.ScreenName {
		t.Errorf("a taken name left the designer on screen %v, want the name screen", m.Screen)
	}
	if m.Err == "" {
		t.Error("a refused write left no message on screen: the operator presses enter again and again")
	}
	if m.Wrote != "" {
		t.Errorf("Wrote = %q after a failed write; the receipt would name a file that is not this one", m.Wrote)
	}
	if m.Quit {
		t.Error("a refused write quit the designer, throwing away everything that was picked")
	}
	if len(m.Picked) != 1 {
		t.Errorf("the picks did not survive the refusal: %+v", m.Picked)
	}
}

// TestAResizeRedrawsWithoutAKeystroke is the only input the loop takes that is
// not a key.
//
// A window that changed size and a frame drawn for the old one is a screen with
// the last few rows of the previous frame still on it, and nothing the operator
// can press to fix it -- every key redraws at the size the loop still believes in.
// So SIGWINCH asks the terminal again and draws, and this test's size function is
// what proves both: it counts the asks, and it answers differently each time so
// the second frame is a measurably different shape.
func TestAResizeRedrawsWithoutAKeystroke(t *testing.T) {
	keys := make(chan designer.Key, 1)
	resize := make(chan os.Signal, 1)
	resize <- syscall.SIGWINCH

	asked := 0
	size := func() (int, int) {
		asked++
		if asked == 2 {
			// The resize has been taken. Let a key end the loop, so this runs
			// without a timer and without a second goroutine to race.
			keys <- designer.Press(designer.KeyInterrupt)
		}
		return 40, 20 - asked
	}

	var out writes
	session{keys: keys, resize: resize, out: &out, size: size}.run(designer.New(nil))

	if asked != 2 {
		t.Fatalf("the terminal was asked for its size %d times, want 2: once at the start and once per resize", asked)
	}
	if len(out.parts) != 2 {
		t.Fatalf("drew %d frames, want 2: the resize alone should have drawn one", len(out.parts))
	}
	for i, want := range []int{18, 17} { // h-1 row breaks in a frame of h rows
		if got := strings.Count(out.parts[i], "\r\n"); got != want {
			t.Errorf("frame %d has %d row breaks, want %d: it was drawn for the wrong window", i, got, want)
		}
	}
}

// TestOneFrameIsOneWriteOfExactlyTheWindow is the contract between Render and the
// terminal, checked on the bytes rather than on the Frame.
//
// Three things are being asserted at once, and each of them is a visible bug when
// it breaks. One write, because a frame delivered in pieces can be interleaved
// with a panic traceback or torn across a refresh. \r\n between rows and never a
// bare \n, because in raw mode a line feed moves down without returning, and the
// second row would start under the end of the first. And no newline after the last
// row, because that one scrolls the whole frame up by a line every single draw.
func TestOneFrameIsOneWriteOfExactlyTheWindow(t *testing.T) {
	var out writes
	session{out: &out}.draw(designer.New(nil), 40, 12)

	if len(out.parts) != 1 {
		t.Fatalf("one frame took %d writes, want 1", len(out.parts))
	}
	f := out.parts[0]
	if n := strings.Count(f, "\r\n"); n != 11 {
		t.Errorf("a frame of 12 rows has %d row breaks, want 11", n)
	}
	if n := strings.Count(f, "\n"); n != 11 {
		t.Errorf("%d line feeds for 11 row breaks: one of them has no carriage return in front of it", n)
	}
	if strings.HasSuffix(f, "\n") {
		t.Error("the frame ends with a newline, which scrolls the window up one row per draw")
	}
	if !strings.HasPrefix(f, hideCursor+cursorHome) {
		t.Error("a frame that does not start by hiding the cursor and going home draws itself down the screen")
	}
	if n := strings.Count(f, clearLine); n != 12 {
		t.Errorf("%d rows clear to end of line, want 12: a shorter row leaves the old one's tail behind", n)
	}
	if !strings.HasSuffix(f, showCursor) {
		t.Error("the name screen has a text field and the frame left the cursor hidden")
	}

	// And the screens that ask nothing leave it hidden, so a block does not sit
	// on the receipt in whatever spot the last frame happened to end on.
	done := designer.New(nil)
	done.Screen, done.Wrote = designer.ScreenDone, "agents/release.yaml"
	var after writes
	session{out: &after}.draw(done, 40, 12)
	if strings.Contains(after.String(), showCursor) {
		t.Error("the receipt screen shows the cursor, and there is nothing on it to type")
	}
}

// TestNothingButATerminalGetsRawMode is the check that makes the refusal in
// cmdDesign correct rather than lucky.
//
// The tempting test for "is this a terminal" is the file's mode, and it is wrong
// in the exact case that matters: /dev/null is a character device, so a mode check
// calls it a terminal and the designer draws a full screen into it, forever, with
// no keys ever arriving. The ioctl is the only thing that actually knows, and it
// answers ENOTTY straight away.
func TestNothingButATerminalGetsRawMode(t *testing.T) {
	scratch := t.TempDir() + "/regular"
	if err := os.WriteFile(scratch, []byte("not a terminal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ what, path string }{
		{"the null device, which is a character device just like a terminal", os.DevNull},
		{"a regular file, which is what a redirect gives", scratch},
	} {
		t.Run(tc.what, func(t *testing.T) {
			f, err := os.Open(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if restore, err := rawMode(int(f.Fd())); err == nil {
				restore()
				t.Errorf("rawMode succeeded on %s: the designer would draw into it and wait for keys forever", tc.path)
			}
			if w, h, ok := windowSize(int(f.Fd())); ok {
				t.Errorf("windowSize(%s) = %d, %d, true; nothing there has a size", tc.path, w, h)
			}
		})
	}
}

// TestTheDesignerRefusalReadsAsARefusalToRunAndNotAsAGap is about the sentence,
// not the behaviour.
//
// `arxi design` in a pipe, in CI, or under a here-doc cannot run, and there are
// two very different things that message could sound like: a command that is not
// finished, or a command that needs a screen. It has to be the second one. So it
// carries neither the not-implemented sentinel nor any of the phrases isRefusal
// looks for -- if it did, the surface tests would count design as missing again
// the moment it was finished -- and it names both of the things an operator in a
// pipe can do instead, since telling somebody their terminal is wrong without
// telling them what does work is a dead end.
func TestTheDesignerRefusalReadsAsARefusalToRunAndNotAsAGap(t *testing.T) {
	msg := designNeedsTerminal(syscall.ENOTTY)

	if isRefusal(msg) {
		t.Errorf("the terminal refusal reads like an unimplemented command:\n%s", msg)
	}
	if strings.Contains(msg, notImplementedSentinel) {
		t.Errorf("the terminal refusal carries the not-implemented sentinel:\n%s", msg)
	}
	for _, want := range []string{
		"not a terminal",        // what went wrong
		"arxi blueprint create", // the same file, without a screen
		"arxi agent list",       // what there is to pick from
		syscall.ENOTTY.Error(),  // the errno, for the case that is not a pipe
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the terminal refusal never mentions %q:\n%s", want, msg)
		}
	}
}

// TestTheReceiptCanNameTheFileEachMemberCameFrom is what sourcePaths is for.
//
// printTeamCreated looks a member up by the name it will be addressed by, which
// is not always the name of the file it came from: agents/legacy-api.yaml can
// declare a member called api, and a receipt keyed by filename would silently
// have nothing to say about it. Composing a team is the one operation where the
// operator needs to see which file each member arrived from, because two files
// with the same member name is the mistake this screen exists to catch.
func TestTheReceiptCanNameTheFileEachMemberCameFrom(t *testing.T) {
	legacy := storedAgent("legacy-api", "test-model")
	legacy.Blueprint.Config.Members[0].Name = "api"

	m := designer.New(designer.Candidates([]agentstore.Entry{
		storedAgent("planner", "test-model"),
		legacy,
	}))
	m.Picked = []int{1}

	from := sourcePaths(m)
	if len(from) != 1 {
		t.Fatalf("sourcePaths named %d members, want just the picked one: %v", len(from), from)
	}
	if got := from["api"]; got != "agents/legacy-api.yaml" {
		t.Errorf("member api came from %q, want agents/legacy-api.yaml: the receipt is keyed by member name", got)
	}
}
