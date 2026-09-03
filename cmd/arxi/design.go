// design.go is the terminal half of the blueprint designer.
//
// internal/designer is a pure Update/Render pair on purpose, and everything it
// deliberately does not know is here: raw mode, the escape sequences, the real
// cursor, the window size, the signal that says the window changed, and the
// CreateTeam call that turns the last screen into a file. internal/arch_test.go
// enforces that split in both directions -- the designer may not import os, and
// it may not call CreateTeam -- so this is the only file where either appears.
//
// The loop is deliberately dull: take a key, call Update, perform whatever
// Command comes back, draw the frame Render returns. Every decision the operator
// sees was made in the pure package, where it is asserted at nine window sizes
// without a terminal in sight.
package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"

	"github.com/michiTrader/arxi/internal/agentstore"
	"github.com/michiTrader/arxi/internal/designer"
	"github.com/michiTrader/arxi/internal/surface"
)

// cmdDesign is `arxi design`: compose a blueprint by answering four screens.
//
// The order of the first three steps is the whole safety argument of this
// command. --help is answered before anything touches the terminal, the store is
// read before raw mode is entered, and raw mode is entered before a single byte
// is drawn -- so every way this can fail early fails on an ordinary screen with
// an ordinary message, and nothing that has to be undone has been done yet.
func cmdDesign(args []string) {
	vals, err := parseInvocation(surface.Lookup("design"), args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi design: %v\n", err)
		os.Exit(2)
	}

	// parseInvocation synthesises --json for every command that does not mutate,
	// which is right for the reading verbs and empty here: this command's entire
	// output is a screen. Refusing beats accepting-and-ignoring, because ignoring
	// a flag reports success for a request that was never carried out, and the
	// refusal names the two spellings that do print machine-readable output.
	if _, ok := vals["json"]; ok {
		fmt.Fprint(os.Stderr, "arxi design: --json has nothing to print.\n"+
			"  this command's output is a full-screen designer driven with the arrow keys,\n"+
			"  and it ends by writing one blueprint file.\n\n"+
			"  compose a team without a screen: arxi blueprint create <name> --members a,b\n"+
			"  read the store as JSON:          arxi agent list --json\n")
		os.Exit(2)
	}

	// The listing is read once, before raw mode, because it is the only part of
	// this command that can fail in a way worth reading: an unreadable agents/, a
	// file that will not parse. A diagnostic printed after the alternate screen is
	// up gets drawn over by the next frame and then thrown away with the buffer.
	//
	// readAgents and not openAgents: opening the designer must not create the
	// store. A missing directory is a store with nobody in it, which is a screen
	// the designer already draws -- it says so, and names `arxi agent create`.
	entries, err := readAgents().List()
	if err != nil {
		fatal(err)
	}
	m := designer.New(designer.Candidates(entries))

	in, out := os.Stdin, os.Stdout
	restore, err := rawMode(int(in.Fd()))
	if err != nil {
		fmt.Fprint(os.Stderr, designNeedsTerminal(err))
		os.Exit(2)
	}

	// leave puts the terminal back the way it was found. It is safe to call more
	// than once, and it has to be, because more than one thing can decide this
	// command is over: the loop returning, a panic unwinding, a signal that would
	// otherwise kill the process, and fatal() -- which openAgents() calls, from
	// inside the write, with the alternate screen already up.
	//
	// Registered three ways for those four callers. defer covers the panic, so the
	// traceback lands on a restored screen instead of a raw one where every line
	// starts under the end of the last. atExit covers fatal(), which ends the
	// process through os.Exit and therefore runs no defers at all. A terminal left
	// in raw mode is worse than the stranded lock file that put that hook registry
	// here: the shell it strands has no echo, and nothing points at arxi.
	var once sync.Once
	leave := func() {
		once.Do(func() {
			io.WriteString(out, showCursor+altScreenOff)
			restore()
		})
	}
	defer leave()
	atExit(leave)

	// SIGWINCH is the only notice that the window changed size, and a frame drawn
	// at the old size is the corrupted display Frame's doc describes: too few rows
	// leaves the previous frame's rows underneath, too many wrap and scroll the
	// title away. Buffered by one because the size is re-read after the signal, so
	// a second one that arrives while the first is being handled is already stale.
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)

	// A signal that would kill the process has to restore the terminal on the way
	// out. Without this, closing the window or `kill <pid>` leaves the shell in raw
	// mode inside the alternate screen -- a shell that looks broken, in a session
	// where the operator did nothing wrong.
	//
	// os.Interrupt is in the list even though ISIG is off and ctrl+c arrives as a
	// key: this process can still be sent one by something that is not the
	// keyboard. 128+n is the exit code a shell reports for a signal, and reporting
	// the real one keeps `arxi design; echo $?` honest.
	dying := make(chan os.Signal, 1)
	signal.Notify(dying, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		s := <-dying
		leave()
		code := 1
		if n, ok := s.(syscall.Signal); ok {
			code = 128 + int(n)
		}
		os.Exit(code)
	}()

	// The alternate screen buffer, so what was in the terminal before this command
	// is still there after it. The operator's scrollback is theirs, and it very
	// likely holds the `arxi agent list` they ran to decide to open this.
	io.WriteString(out, altScreenOn)

	// The size is asked of stdout when that is where the frame goes, and of stdin
	// when stdout is not a terminal at all: `arxi design > frames.txt` is a strange
	// thing to do, and it still has a real terminal to measure. 80x24 is the last
	// resort rather than the first guess.
	size := func() (int, int) {
		if w, h, ok := windowSize(int(out.Fd())); ok {
			return w, h
		}
		if w, h, ok := windowSize(int(in.Fd())); ok {
			return w, h
		}
		return 80, 24
	}

	m = session{
		keys:   keyStream(in),
		resize: resize,
		out:    out,
		size:   size,
		write: func(t agentstore.Team) (string, error) {
			// openAgents here and readAgents above: this is the moment the store
			// has to exist, and the only moment, so a designer that is quit
			// without writing leaves a fresh installation exactly as it was.
			return openAgents().CreateTeam(t)
		},
	}.run(m)
	leave()

	// The receipt is printed on the ordinary screen, after the alternate one is
	// gone, and it is `blueprint create`'s receipt rather than a second one of its
	// own. The two verbs compose the same file, so they owe the same warnings --
	// the all-advisory team that goes quiescent after zero turns, the memberless
	// model that makes a live run need --model -- and the same next command. It
	// also survives being scrolled back to, which nothing on the alternate screen
	// does.
	if m.Wrote == "" {
		// A full-screen command that vanishes leaving no trace is a command the
		// operator has to go and check on. One line is cheaper than that trip.
		fmt.Fprintln(os.Stderr, "arxi design: quit without writing anything.")
		return
	}
	printTeamCreated(m.Team(), m.Wrote, sourcePaths(m))
}

// sourcePaths is member name -> the file that member was copied out of.
//
// printTeamCreated takes this separately because the finished team cannot answer
// the question: composing it copies the member config and drops the path. It is
// the fact that makes a wrong pick findable after the file exists, and it is not
// derivable from the name -- `agent create` writing member name == file name is a
// convention, and a hand-edited file is free to disagree with it.
func sourcePaths(m designer.Model) map[string]string {
	from := make(map[string]string, len(m.Picked))
	for _, i := range m.Picked {
		c := m.Cands[i]
		from[c.Member.Name] = c.Path
	}
	return from
}

// designNeedsTerminal is what this command says when stdin is not one.
//
// The wording is load-bearing twice. surface_coverage_test.go decides whether a
// declared capability is implemented by looking for the one sentence
// notImplemented prints, and model_cli_test.go decides whether the usage screen
// must advertise `design` by looking for four phrases; this message contains
// neither, on purpose, because refusing to run in a pipe is not the same fact as
// not being built. TestTheDesignerRefusalReadsAsARefusalToRunAndNotAsAGap keeps
// it that way.
//
// It also has to arrive instantly, which is why the detector is the ioctl rather
// than a look at what stdin is. Under `go test` this command's stdin is
// /dev/null, and /dev/null is a character device: every heuristic based on the
// file mode answers "terminal", and the designer would then sit waiting for a
// keystroke that cannot arrive until the test binary's own deadline killed it.
func designNeedsTerminal(err error) string {
	return fmt.Sprintf("arxi design draws a full-screen designer, and stdin here was "+
		"not a terminal.\n  (asking the terminal for its settings: %v)\n\n"+
		"  run it from a shell rather than from a pipe, a here-doc, or CI.\n"+
		"  the same file without a screen: arxi blueprint create <name> --members a,b\n"+
		"  what there is to pick from:     arxi agent list\n", err)
}

// The escape sequences this command sends, and all of them there are.
//
// They live here and not in internal/designer because that package is asserted
// to contain no 0x1b at all: a sequence built into a frame line is a sequence
// whose effect that package cannot see, and a test of it there would be checking
// bytes where it means to check a screen.
//
// None is conditional on TERM. The fallback for a terminal that does not
// understand them is a screen drawn in the wrong places, which is not a
// fallback -- on such a terminal the answer is `arxi blueprint create`.
const (
	altScreenOn  = "\x1b[?1049h" // the alternate buffer: scrollback is left alone
	altScreenOff = "\x1b[?1049l" // back to it, with what was on screen still there
	hideCursor   = "\x1b[?25l"
	showCursor   = "\x1b[?25h"
	cursorHome   = "\x1b[H" // row 1, column 1
	clearLine    = "\x1b[K" // erase from the cursor to the end of this row
)

// session is everything the loop needs from outside the pure package, gathered
// into one struct so that a test can drive the whole of it with a strings.Reader,
// a bytes.Buffer, and a function that pretends to write a file.
//
// That is not test convenience for its own sake. The alternative is a pty, and a
// pty test mostly proves that the terminal works. What can actually go wrong in
// here is ordering -- a command performed and its outcome never fed back, a frame
// drawn from the model before the one that produced it -- and these five fields
// are what make that visible from a test.
type session struct {
	keys   <-chan designer.Key
	resize <-chan os.Signal // nil under test: a nil channel in select never fires
	out    io.Writer
	size   func() (w, h int)
	write  func(agentstore.Team) (string, error)
}

// run drives the model to its end and reports the model it stopped on, so the
// caller can print a receipt about what was written rather than about what it
// hopes was written.
//
// The first frame goes out before the first key is read, because a screen that
// appears only once something has been typed is indistinguishable from a command
// that has hung.
func (s session) run(m designer.Model) designer.Model {
	w, h := s.size()
	s.draw(m, w, h)
	for {
		var cmds []designer.Command
		select {
		case k, ok := <-s.keys:
			if !ok {
				// stdin ended: the terminal is gone, or a test's script ran
				// out. Returning is the only honest answer, because there is
				// no longer any way to ask the operator anything.
				return m
			}
			m, cmds = designer.Update(m, designer.KeyPress{Key: k})
		case <-s.resize:
			w, h = s.size()
		}
		// A command's outcome goes back through Update, and this keeps going
		// until nothing new comes out. Today the only pair is Write -> Written
		// and Written asks for nothing, so it runs once; it is a loop because
		// draining one level would, on the day an outcome produces a command,
		// perform that command and throw its result away.
		for len(cmds) > 0 {
			var next []designer.Command
			for _, c := range cmds {
				var more []designer.Command
				m, more = s.perform(m, c)
				next = append(next, more...)
			}
			cmds = next
		}
		if m.Quit {
			return m
		}
		s.draw(m, w, h)
	}
}

// perform carries out one Command and feeds the outcome straight back in.
//
// There is one Command, and this is the only place in the binary where the
// designer's model becomes a file. The outcome is handed to Update rather than
// dealt with here because what a failed write MEANS is a screen decision: a name
// already taken sends the operator back to the name field with the refusal under
// it, and anything else keeps them on review, where esc still works. update.go
// decides which, and is tested on it.
func (s session) perform(m designer.Model, c designer.Command) (designer.Model, []designer.Command) {
	switch c := c.(type) {
	case designer.Write:
		path, err := s.write(c.Team)
		return designer.Update(m, designer.Written{Path: path, Err: err})
	default:
		// Unreachable, and put on the screen rather than dropped: it can only
		// mean the pure package grew a Command and this file was not taught to
		// carry it out. m.Err is where the frame already shows refusals, which
		// beats a line written to stderr underneath a full-screen frame.
		m.Err = fmt.Sprintf("nothing in arxi performs a %T command (internal error)", c)
		return m, nil
	}
}

// draw writes one frame, in one Write, with the cursor hidden while it goes out.
//
// One Write because a frame delivered in pieces can be interleaved with whatever
// else has the terminal; hidden because the terminal's own cursor would otherwise
// be visible walking every row as it is painted, which reads as flicker.
//
// \r\n between rows and not \n: raw mode leaves the kernel's newline translation
// out of it, so \n moves down one row and stays in the column it was already in,
// and every row would start under the end of the row above. There is deliberately
// no newline after the LAST row -- on a frame that fills the window it scrolls
// everything up by one and takes the title row with it.
//
// Each row ends with an erase-to-end-of-row instead of being padded out to w
// spaces: it clears whatever the previous frame left to the right of this one at
// any width, and costs three bytes rather than up to w.
func (s session) draw(m designer.Model, w, h int) {
	f := designer.Render(m, w, h)

	var b strings.Builder
	b.WriteString(hideCursor)
	b.WriteString(cursorHome)
	for i, l := range f.Lines {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(l)
		b.WriteString(clearLine)
	}
	// The cursor is placed last, once the rows it sits on are already there, and
	// shown only on the screens Render says have a field to type in.
	if f.ShowCursor {
		fmt.Fprintf(&b, "\x1b[%d;%dH", f.Cursor.Row+1, f.Cursor.Col+1)
		b.WriteString(showCursor)
	}
	io.WriteString(s.out, b.String())
}

// keyStream turns a reader of bytes into a channel of keys.
//
// A channel rather than a Read inside the loop, because the loop also has to hear
// SIGWINCH and select is the only way to wait on both. When the loop returns
// first this goroutine is left blocked on a read nobody will answer; the process
// is on its way out, and arranging for it to notice would buy nothing.
//
// pending survives across reads because the kernel can split an escape sequence
// at any byte: `\x1b[` in one read and `A` in the next is one up arrow, and a
// decoder that could only see a single read would report Esc, then `[`, then `A`
// -- which on the name screen goes back a screen and types two letters.
func keyStream(r io.Reader) <-chan designer.Key {
	out := make(chan designer.Key)
	go func() {
		defer close(out)
		var pending []byte
		buf := make([]byte, 256)
		for {
			n, err := r.Read(buf)
			pending = append(pending, buf[:n]...)
			for len(pending) > 0 {
				k, used, ok := decode(pending)
				if used == 0 {
					break // incomplete: the rest is in the next read
				}
				pending = pending[used:]
				if ok {
					out <- k
				}
			}
			// A lone ESC cannot be told apart from the start of a sequence whose
			// tail has not arrived, and it is the back-or-quit key on every
			// screen. Waiting for more would leave it dead until the NEXT
			// keypress, so a read that ends on exactly one ESC is taken as the
			// key. The price is that a sequence split precisely after its ESC
			// reads as Esc plus its own tail; the alternative is a timer, which
			// trades a rare wrong key for a permanent guess about how fast the
			// terminal is.
			if len(pending) == 1 && pending[0] == esc {
				pending = nil
				out <- designer.Press(designer.KeyEsc)
			}
			if err != nil {
				return
			}
		}
	}()
	return out
}

const esc = 0x1b

// decode reads the first key out of b, and the caller depends on all three of its
// outcomes.
//
// used == 0 means what is there is the beginning of something whose rest has not
// arrived, and nothing may be consumed. ok == false with used > 0 means those
// bytes were something this designer has no answer for, and dropping them IS the
// answer: a terminal sends sequences nobody asked for -- a mouse report, a
// bracketed-paste marker, a status reply -- and they must not arrive as the
// letters they are spelled with.
func decode(b []byte) (designer.Key, int, bool) {
	if len(b) == 0 {
		return designer.Key{}, 0, false
	}
	switch c := b[0]; {
	case c == esc:
		return decodeEsc(b)
	case c == 0x03:
		return designer.Press(designer.KeyInterrupt), 1, true
	case c == '\r', c == '\n':
		// Both, because which one arrives depends on the terminal: ICRNL is off
		// in raw mode, so the return key is the \r it always was, and a script
		// piped in is full of \n.
		return designer.Press(designer.KeyEnter), 1, true
	case c == '\t':
		return designer.Press(designer.KeyTab), 1, true
	case c == 0x7f, c == 0x08:
		// 0x7f from almost every terminal, 0x08 from the ones that send what
		// ctrl+h sends. Backspace deletes the rune before the cursor on two
		// screens and undoes the last stage on a third; being deaf to it on one
		// terminal in ten would make the field feel broken.
		return designer.Press(designer.KeyBackspace), 1, true
	case c < 0x20:
		// Every other control byte. ctrl+g means nothing on any of these
		// screens, and letting it through as its letter would type a `g` into
		// the name field for a key nobody pressed.
		return designer.Key{}, 1, false
	}
	if !utf8.FullRune(b) {
		return designer.Key{}, 0, false // half a rune: wait for the rest of it
	}
	if r, n := utf8.DecodeRune(b); r != utf8.RuneError || n > 1 {
		return designer.Typed(r), n, true
	}
	return designer.Key{}, 1, false // not the start of any rune at all
}

// decodeEsc reads one escape sequence, or says that not all of it is here yet.
func decodeEsc(b []byte) (designer.Key, int, bool) {
	if len(b) < 2 {
		return designer.Key{}, 0, false
	}
	switch b[1] {
	case esc:
		// ESC ESC: the key pressed twice, or a terminal that doubles it to mean
		// a meta key. One Esc, one byte consumed, and the second ESC is left in
		// the buffer to be decoded on its own terms.
		return designer.Press(designer.KeyEsc), 1, true
	case '[':
		return decodeCSI(b)
	case 'O':
		// SS3: how a terminal in application-keypad mode sends the arrows and
		// Home/End. xterm does it, and so does the Linux console.
		if len(b) < 3 {
			return designer.Key{}, 0, false
		}
		if k, ok := finalKey(b[2]); ok {
			return k, 3, true
		}
		return designer.Key{}, 3, false
	}
	// ESC + anything else is alt+that, which no screen answers to. Both bytes go,
	// not just the ESC: leaving the letter behind would type it into the field.
	if !utf8.FullRune(b[1:]) {
		return designer.Key{}, 0, false
	}
	_, n := utf8.DecodeRune(b[1:])
	return designer.Key{}, 1 + n, false
}

// decodeCSI reads `\x1b[` and whatever follows it.
//
// The sequence is SCANNED the way the standard says one is built -- parameter
// bytes 0x30..0x3f, then intermediates 0x20..0x2f, then one final byte 0x40..0x7e
// -- rather than matched against the handful this designer wants. That is the
// difference between an unknown sequence being dropped whole and its tail being
// typed into the name field: `\x1b[1;5A` is ctrl+up from a terminal that reports
// modifiers, and a matcher looking for `\x1b[A` would consume three of its bytes
// and then type `;5A`.
//
// The arrows come from the final byte whatever the parameters say, so ctrl+up
// moves the cursor exactly like up. On these four screens no modifier means
// anything different from the key it modifies, and doing nothing at all would be
// the one outcome the operator cannot explain.
func decodeCSI(b []byte) (designer.Key, int, bool) {
	i := 2
	for i < len(b) && b[i] >= 0x30 && b[i] <= 0x3f {
		i++
	}
	params := string(b[2:i])
	for i < len(b) && b[i] >= 0x20 && b[i] <= 0x2f {
		i++
	}
	if i == len(b) {
		return designer.Key{}, 0, false // the final byte has not arrived
	}
	final, used := b[i], i+1
	if final < 0x40 || final > 0x7e {
		// Not a final byte, so this was never a sequence -- a stray `\x1b[` with
		// something else behind it. What was scanned is consumed anyway, because
		// leaving it would wedge the stream on bytes nothing can ever decode.
		return designer.Key{}, used, false
	}
	if k, ok := finalKey(final); ok {
		return k, used, true
	}
	if final == '~' {
		// The numbered keys, of which three mean something here. Home and End
		// have two spellings each depending on what the terminal thinks it is.
		switch csiNumber(params) {
		case 1, 7:
			return designer.Press(designer.KeyHome), used, true
		case 4, 8:
			return designer.Press(designer.KeyEnd), used, true
		case 3:
			return designer.Press(designer.KeyDelete), used, true
		}
	}
	return designer.Key{}, used, false
}

// finalKey maps the final byte that CSI and SS3 share onto a key.
//
// A..D are the arrows in both forms, H and F are Home and End, and Z is shift+tab
// -- which arrives as an escape sequence bearing no relation to tab's own byte,
// and is how the members screen goes back a question without losing the picks.
func finalKey(c byte) (designer.Key, bool) {
	switch c {
	case 'A':
		return designer.Press(designer.KeyUp), true
	case 'B':
		return designer.Press(designer.KeyDown), true
	case 'C':
		return designer.Press(designer.KeyRight), true
	case 'D':
		return designer.Press(designer.KeyLeft), true
	case 'H':
		return designer.Press(designer.KeyHome), true
	case 'F':
		return designer.Press(designer.KeyEnd), true
	case 'Z':
		return designer.Press(designer.KeyBackTab), true
	}
	return designer.Key{}, false
}

// csiNumber is the first parameter of a CSI sequence, and 0 when there is not one.
//
// It stops at the first byte that is not a digit, which drops the modifier a
// terminal appends: `\x1b[3~` is Delete and `\x1b[3;5~` is ctrl+delete, and both
// delete the rune under the cursor here. A sequence whose parameters open with a
// private marker -- `\x1b[<0;1;1M`, a mouse report -- yields 0 and is dropped,
// which is the point.
func csiNumber(params string) int {
	n := 0
	for _, c := range params {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}
