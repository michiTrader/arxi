package designer

import (
	"errors"
	"fmt"

	"github.com/michiTrader/arxi/internal/agentstore"
)

// KeyCode is one key, named by what it means rather than by the bytes that
// carried it.
//
// The decoding stays in cmd/arxi because it is a fact about terminals, not about
// designing a team: Home arrives as three different sequences depending on what
// the terminal believes it is, backspace arrives as 0x7f from most of them and
// 0x08 from some, and shift+tab is an escape sequence with no relation to tab.
// None of that belongs in a package whose tests are about what `esc` does on the
// fourth screen.
type KeyCode int

const (
	KeyRune KeyCode = iota // an ordinary character; Rune says which
	KeyEnter
	KeyEsc
	KeyTab
	KeyBackTab // shift+tab
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyHome
	KeyEnd
	KeyBackspace
	KeyDelete
	KeyInterrupt // ctrl+c: leave now, having written nothing
)

// Key is one keystroke. Rune is meaningful only when Code is KeyRune.
type Key struct {
	Code KeyCode
	Rune rune
}

// Typed and Press build the two kinds of Key. They exist because a test that
// says Typed('a'), Press(KeyEnter) reads like the session it describes, and
// because the adapter should not have to remember that Rune is ignored.
func Typed(r rune) Key    { return Key{Code: KeyRune, Rune: r} }
func Press(c KeyCode) Key { return Key{Code: c} }

// Input is everything that can move the designer forward: a keystroke, or the
// store's answer to the one thing the designer asks it to do.
//
// Two kinds and not one, because the write is the only step that can fail for a
// reason no screen could have predicted. Everything else is decided here from
// what is already in the model.
type Input interface{ input() }

// KeyPress is one keystroke, already decoded.
type KeyPress struct{ Key Key }

// Written is what came back from the write, and it carries the error rather than
// a string on purpose: errors.Is decides which screen the operator lands on, and
// a message cannot be asked that question.
type Written struct {
	Path string // where the blueprint was written, when Err is nil
	Err  error
}

func (KeyPress) input() {}
func (Written) input()  {}

// Command is work the designer cannot do itself.
//
// There is exactly one, and that is the claim of the package: the designer's
// whole output is a team handed to agentstore.CreateTeam. Quitting is m.Quit and
// not a command, because the adapter has to restore the terminal on every exit
// path anyway -- ctrl+c, an error, a closed pipe -- so a Quit command would be a
// second place that decides an already-decided fact.
type Command interface{ command() }

// Write asks for the reviewed team to become a file. The adapter calls
// CreateTeam and feeds the result back as Written.
type Write struct{ Team agentstore.Team }

func (Write) command() {}

// Update folds one input into the model. It is Decide with keystrokes where the
// events go: same signature, same purity, same reason.
//
// An input this function does not recognise leaves the model exactly as it was.
// That is the behaviour a terminal needs -- an unmapped key on a screen that
// does not use it must do nothing at all, not redraw, not clear the refusal
// underneath, and above all not advance.
func Update(m Model, in Input) (Model, []Command) {
	switch in := in.(type) {
	case KeyPress:
		return key(m, in.Key)
	case Written:
		return written(m, in), nil
	}
	return m, nil
}

// written is where the designer ends, one way or the other.
//
// A refusal sends the operator back to the screen that can FIX it, which is why
// Written keeps a typed error. ErrExists means the name was taken between the
// listing and the write -- the one refusal review could not have caught, since
// the candidates are read once before raw mode -- and the name is on the first
// screen. Anything else (a full disk, a read-only directory, a member the store
// refuses) is about the team as a whole, so review is where it belongs and where
// the message is next to the thing it is about.
func written(m Model, w Written) Model {
	if w.Err == nil {
		m.Screen, m.Wrote, m.Err = ScreenDone, w.Path, ""
		return m
	}
	m.Err = oneLine(w.Err.Error())
	if errors.Is(w.Err, agentstore.ErrExists) {
		m.Screen = ScreenName
		return m
	}
	m.Screen = ScreenReview
	return m
}

// key routes a keystroke to the screen that is asking the question.
//
// ctrl+c is answered before the screens get a look at it. It means the same
// thing everywhere, including on the review screen where enter would have
// written a file, and a screen that could swallow it would be a designer you
// cannot leave.
func key(m Model, k Key) (Model, []Command) {
	if k.Code == KeyInterrupt {
		m.Quit = true
		return m, nil
	}
	switch m.Screen {
	case ScreenName:
		return nameKey(m, k), nil
	case ScreenMembers:
		return membersKey(m, k), nil
	case ScreenStages:
		return stagesKey(m, k), nil
	case ScreenReview:
		return reviewKey(m, k)
	case ScreenDone:
		// A receipt, not a question. Any key leaves, because there is nothing
		// here to get wrong and the file already exists.
		m.Quit = true
		return m, nil
	}
	return m, nil
}

// edit applies the keys that belong to a line of text, and reports whether the
// key was one of them.
//
// Shared by the name and the stage field so that the two behave identically. A
// designer where Home works in one text box and not the other is not a smaller
// designer, it is a broken one.
func edit(f field, k Key) (field, bool) {
	switch k.Code {
	case KeyRune:
		return f.insert(k.Rune), true
	case KeyBackspace:
		return f.backspace(), true
	case KeyDelete:
		return f.del(), true
	case KeyLeft:
		return f.left(), true
	case KeyRight:
		return f.right(), true
	case KeyHome:
		return f.home(), true
	case KeyEnd:
		return f.end(), true
	}
	return f, false
}

// nameRefusal is why this cannot be the team's name, or "" if it can.
//
// ValidateName is asked rather than reimplemented, so the designer refuses
// exactly the names the writer would: `..` , a name with a slash in it, a name
// with a trailing space that cannot be typed back. The collision check is the
// designer's own, and it is most of the argument for this screen: `blueprint
// create platform --members a,b` discovers that platform exists after the whole
// command line has been typed, and here it is one screen in with the name still
// under the cursor.
func (m Model) nameRefusal() string {
	name := m.Name.String()
	if err := agentstore.ValidateName("blueprint", name); err != nil {
		return oneLine(err.Error())
	}
	if path := m.taken(name); path != "" {
		return fmt.Sprintf("%s already exists: CreateTeam will refuse this name, "+
			"and overwriting it would take a file somebody may have grown by hand", path)
	}
	return ""
}

// nameKey: the first screen, which is one text field and two ways out.
//
// esc quits instead of going back, because there is nothing behind this screen.
// It is also the key an operator reaches for on arriving somewhere they did not
// mean to be, and the honest answer to that is to leave having written nothing.
func nameKey(m Model, k Key) Model {
	switch k.Code {
	case KeyEnter, KeyTab:
		if why := m.nameRefusal(); why != "" {
			m.Err = why
			return m
		}
		m.Screen, m.Cursor, m.Err = ScreenMembers, 0, ""
	case KeyEsc:
		m.Quit = true
	case KeyBackTab:
		// Nothing is behind the first screen, so this is not a way out. Doing
		// nothing is the whole handling: no advance, no cleared refusal.
	default:
		if f, ok := edit(m.Name, k); ok {
			m.Name, m.Err = f, ""
		}
	}
	return m
}

// membersKey: the store's own listing, with a cursor on it.
//
// The cursor moves over the unusable entries too. They are listed to be read --
// a team is listed with the members to pick instead of it, a half-written file
// with the loader's complaint -- and a row you cannot put the cursor on is a row
// whose reason the operator has to guess at.
func membersKey(m Model, k Key) Model {
	switch k.Code {
	case KeyUp:
		if m.Cursor > 0 {
			m.Cursor, m.Err = m.Cursor-1, ""
		}
	case KeyDown:
		if m.Cursor < len(m.Cands)-1 {
			m.Cursor, m.Err = m.Cursor+1, ""
		}
	case KeyHome:
		m.Cursor, m.Err = 0, ""
	case KeyEnd:
		if len(m.Cands) > 0 {
			m.Cursor = len(m.Cands) - 1
		}
		m.Err = ""
	case KeyRune:
		if k.Rune == ' ' {
			return toggle(m)
		}
		// Every other character. Not a filter box: the listing is what `agent
		// list` prints, and a designer that hid rows as you typed would be
		// hiding the ones you are about to be told you cannot use.
	case KeyEnter, KeyTab:
		if len(m.Picked) == 0 {
			m.Err = "nobody is picked: space picks the agent under the cursor. " +
				"a blueprint with no members loads and validates, and then records " +
				"run.quiescent after zero turns"
			return m
		}
		m.Screen, m.Err = ScreenStages, ""
	case KeyEsc, KeyBackTab:
		m.Screen, m.Err = ScreenName, ""
	}
	return m
}

// toggle picks the agent under the cursor, or unpicks it.
//
// Both refusals here are refusals `blueprint create` also makes, moved to the
// keystroke that causes them. The duplicate-name check is written out rather
// than left to Team.Validate because it can name both FILES: a composed member
// records nothing about where it came from, so by the time the team is a value
// the two paths are gone -- and `backend.yaml and legacy/backend.yaml both call
// it api` is the sentence that tells the operator which one to drop.
func toggle(m Model) Model {
	if len(m.Cands) == 0 {
		m.Err = "there are no agents to pick: `arxi agent create <name> --model <id> " +
			"--tools read` writes one, and this screen lists what it wrote"
		return m
	}
	i, c := m.Cursor, m.Cands[m.Cursor]
	if m.isPicked(i) {
		// Unpicking cannot fail, and it must not consult Unusable: a file that
		// somehow got picked has to be droppable.
		out := make([]int, 0, len(m.Picked))
		for _, p := range m.Picked {
			if p != i {
				out = append(out, p)
			}
		}
		m.Picked, m.Err = out, ""
		return m
	}
	if c.Unusable != "" {
		m.Err = c.Name + " " + c.Unusable
		return m
	}
	for _, p := range m.Picked {
		if m.Cands[p].Member.Name == c.Member.Name {
			m.Err = fmt.Sprintf("%s and %s would both call it %q: a stage activates a "+
				"member by name and `run steer <run> <member>` addresses it by name, so "+
				"two of them is a team the kernel cannot tell apart",
				m.Cands[p].Path, c.Path, c.Member.Name)
			return m
		}
	}
	// Copied before appending. Model is a value that tests and the loop both
	// keep older copies of, and append into spare capacity would write through
	// every one of them -- the one way a pure-looking fold can mutate the past.
	m.Picked, m.Err = append(append([]int{}, m.Picked...), i), ""
	return m
}

// stagesKey: a list built one line at a time, with no cursor in it.
//
// enter appends what was typed, or advances when nothing was. That is how every
// prompt that collects a list behaves, and it is why this screen needs no third
// key: the empty field IS the "done" answer. Backspace on an empty field takes
// the last stage back, so the only two keys are the two an operator already has
// their fingers on -- and Model.Cursor keeps one meaning, "the row in Cands",
// instead of meaning something different on every screen.
func stagesKey(m Model, k Key) Model {
	switch k.Code {
	case KeyEnter, KeyTab:
		if m.Stage.empty() {
			m.Screen, m.Err = ScreenReview, ""
			return m
		}
		return addStage(m)
	case KeyBackspace:
		if m.Stage.empty() {
			if len(m.Stages) == 0 {
				// Nothing typed and nothing to take back. Leave the refusal on
				// screen: the key did nothing, so it has answered nothing.
				return m
			}
			// Copied rather than resliced, for the reason toggle copies: a later
			// append would write into the slot an older Model still reads.
			m.Stages = append([]string{}, m.Stages[:len(m.Stages)-1]...)
			m.Err = ""
			return m
		}
	case KeyEsc, KeyBackTab:
		m.Screen, m.Err = ScreenMembers, ""
		return m
	}
	if f, ok := edit(m.Stage, k); ok {
		m.Stage, m.Err = f, ""
	}
	return m
}

// addStage appends the typed stage, or says why it cannot be one.
//
// ValidateStages is asked about the list as it WOULD be, which is what makes the
// duplicate check work: `review` twice is legal as a name and illegal as an
// addition, and only the whole list can tell the difference. The store answers
// both questions, so the designer refuses precisely what CreateTeam refuses --
// and a stage name that reaches review has already been checked by the code that
// will check it again.
func addStage(m Model) Model {
	next := append(append([]string{}, m.Stages...), m.Stage.String())
	if err := agentstore.ValidateStages(next); err != nil {
		m.Err = oneLine(err.Error())
		return m
	}
	m.Stages, m.Stage, m.Err = next, field{}, ""
	return m
}

// reviewKey: the whole team on one screen, and the one key that writes.
//
// tab does not advance here, and this is the only screen where it does not. Tab
// has meant "next question" three times by now, so a tab that wrote a file would
// be a file written by muscle memory. enter is the deliberate key, and it is the
// only one that produces a Command.
//
// Validate runs on exactly the value the Write carries, so nothing can be
// confirmed on this screen and refused by the store afterwards -- except for the
// name collision the store alone can see, which is what written() is for. It is
// the same function CreateTeam calls; running it twice is the point.
func reviewKey(m Model, k Key) (Model, []Command) {
	switch k.Code {
	case KeyEnter:
		team := m.Team()
		if err := team.Validate(); err != nil {
			m.Err = oneLine(err.Error())
			return m, nil
		}
		m.Err = ""
		return m, []Command{Write{Team: team}}
	case KeyEsc, KeyBackTab:
		m.Screen, m.Err = ScreenStages, ""
	}
	return m, nil
}
