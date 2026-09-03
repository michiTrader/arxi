// Package designer is the interactive blueprint designer, with no terminal in it.
//
// The shape is the kernel's, deliberately. Update(Model, Input) -> (Model,
// []Command) is Decide(State, Event, Config) -> (State, []Effect) with
// keystrokes where the events go, and Render is a pure function from a model and
// a window size to the frame that should be on screen. cmd/arxi owns raw mode,
// the escape sequences, the real cursor and the file that gets written.
//
// The reason is the reason the kernel is pure, not a taste for symmetry. A
// designer written the obvious way -- a loop that reads a byte, prints a line
// and calls the store -- can only be exercised by driving a terminal, so the
// cases that actually break it are the cases that never get a test: what the
// screen shows when the store holds forty agents and the window is twenty rows,
// what happens when the name you typed is already on disk, what `esc` means on
// the first screen as opposed to the fourth, whether the frame is still correct
// after a refusal. Here every one of those is a sequence of keys folded into a
// model and one string to assert on.
//
// What the designer is FOR is the other half, and it is worth stating because
// `blueprint create` already composes a team out of stored agents. That verb
// takes the names on the command line, which means two things: you have to know
// them before you start, and every refusal arrives after you have finished
// typing. `--members platform` fails because platform is itself a team; a second
// attempt fails because two of the agents you named carry the same member name;
// a third fails on a `stages:` line in a file you did not write. Each of those is
// a correct refusal delivered at the worst possible moment.
//
// The designer asks the same questions with the store's own answers in front of
// you, and refuses while you are still choosing. A team is listed and marked
// unpickable with the count and the names to use instead. A duplicate member
// name is refused against the file you already picked, by name. The stage
// cross-check runs on the review screen against agentstore.Team.Validate -- the
// same function the writer runs -- so nothing can be confirmed here and refused
// afterwards.
//
// It writes through agentstore.CreateTeam and nothing else. There is no second
// writer, no new file format and no new noun: the designer's output is a
// `blueprint create` whose arguments you did not have to know.
package designer

import (
	"fmt"
	"strings"

	"github.com/michiTrader/arxi/internal/agentstore"
	"github.com/michiTrader/arxi/internal/kernel"
)

// Screen is which of the questions the designer is asking.
//
// Four questions and an ending, in the order a team gets composed: a name, who
// is in it, what stages it has, and a look at the whole thing before it becomes
// a file. The order is not cosmetic -- the members decide which stages are
// legal, because a member may declare stages of its own and Validate checks them
// against the team's -- so stages are asked after members and reviewed after
// both.
type Screen int

const (
	ScreenName    Screen = iota // what is the team called
	ScreenMembers               // who is in it
	ScreenStages                // what stages does it have
	ScreenReview                // is this right
	ScreenDone                  // it exists now, or it was refused
)

func (s Screen) String() string {
	switch s {
	case ScreenName:
		return "name"
	case ScreenMembers:
		return "members"
	case ScreenStages:
		return "stages"
	case ScreenReview:
		return "review"
	case ScreenDone:
		return "done"
	}
	return fmt.Sprintf("screen(%d)", int(s))
}

// Candidate is one stored agent as the picker needs it.
//
// Name is the FILE's name, because that is what `agent list` prints and what the
// operator is looking for. Member.Name is what the composed team will call it,
// and the two need not match: `agent create` writes them equal, a hand edit can
// part them, and every refusal about a duplicate has to be able to say both.
type Candidate struct {
	Name     string              // the file, without the extension
	Path     string              // where it is, for a refusal that names it
	Member   kernel.MemberConfig // what would be copied in; zero when Unusable
	Unusable string              // non-empty: why this file cannot be a member
}

// Candidates turns the store's listing into what the picker offers.
//
// The three reasons a file cannot be a member are resolveMembers' three reasons
// in resolveMembers' order -- it did not load, it has nobody in it, it is itself
// a team -- and they are decided here instead of in the adapter for two reasons.
// One tested place says what "cannot be a member" means, so the designer and
// `blueprint create` cannot drift apart about it. And an unusable file is LISTED
// rather than dropped: a directory of seven agents where two are teams must not
// look like a directory of five, or the operator goes looking for the file they
// know they wrote.
func Candidates(entries []agentstore.Entry) []Candidate {
	out := make([]Candidate, 0, len(entries))
	for _, e := range entries {
		c := Candidate{Name: e.Name, Path: e.Path}
		switch {
		case e.Err != nil:
			// An agent is an ordinary blueprint, so the loader's own words are
			// the useful ones; `blueprint validate <path>` repeats them in full.
			c.Unusable = "did not load: " + oneLine(e.Err.Error())
		case len(e.Blueprint.Config.Members) == 0:
			// blueprint.Load accepts a memberless file on purpose. It is valid,
			// `agent list` shows it, and there is nothing inside it to copy.
			c.Unusable = "loads and has nobody in it to copy"
		case len(e.Blueprint.Config.Members) > 1:
			ms := e.Blueprint.Config.Members
			c.Unusable = fmt.Sprintf("is itself a team of %d (%s) -- pick its members instead",
				len(ms), strings.Join(memberNames(ms), ", "))
		default:
			c.Member = e.Blueprint.Config.Members[0]
		}
		out = append(out, c)
	}
	return out
}

func memberNames(ms []kernel.MemberConfig) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

// oneLine flattens text that is about to become one row of a frame.
//
// The loader's errors are deliberately several lines long, and a newline inside
// a frame line does not wrap -- it shifts every row below it up by one and leaves
// the screen one line short of what Render was asked for.
func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

// Model is the whole designer: everything on screen and everything typed so far.
//
// It is a value with exported fields and no methods that mutate. A test builds
// the model it wants to talk about, folds keys into it and compares the result;
// nothing has to be reset between cases, because nothing was shared.
//
// Err is the refusal shown under the current screen, and it is a string rather
// than an error because it is a thing to print, not a thing to handle. It is
// cleared by the next key that changes anything, so a message never outlives what
// it was about -- a refusal still on screen after the problem is fixed is worse
// than no refusal, because the operator stops believing the line.
type Model struct {
	Screen Screen
	Cands  []Candidate

	Name   field // the team's name, on ScreenName
	Stage  field // the stage being typed, on ScreenStages
	Picked []int // indices into Cands, in the order they were picked
	Stages []string
	Cursor int    // the highlighted row: into Cands, or into Stages
	Err    string // the refusal under the current screen

	Wrote string // the path CreateTeam wrote, on ScreenDone
	Quit  bool   // the loop is over; the adapter restores the terminal
}

// New is the model the designer starts in: the name screen, with the store's
// listing already in hand.
//
// The candidates are read once, before raw mode, and not re-read. A designer that
// re-listed the directory on every keystroke would let a file appear between the
// picker and the write, which is a team composed out of something the operator
// never saw.
func New(cands []Candidate) Model {
	return Model{Screen: ScreenName, Cands: cands}
}

// Members is what would be written, in the order the members were picked.
//
// Pick order and not directory order, because the order in the file is the order
// `agent show` prints and the order a reader assumes is meaningful. Sorting it
// would discard a choice the operator made with two keystrokes.
func (m Model) Members() []kernel.MemberConfig {
	out := make([]kernel.MemberConfig, 0, len(m.Picked))
	for _, i := range m.Picked {
		out = append(out, m.Cands[i].Member)
	}
	return out
}

// Team is the value the store will be asked to write. Review runs Validate on
// exactly this, so what is confirmed and what is written cannot differ.
func (m Model) Team() agentstore.Team {
	return agentstore.Team{Name: m.Name.String(), Members: m.Members(), Stages: m.Stages}
}

func (m Model) isPicked(i int) bool {
	for _, p := range m.Picked {
		if p == i {
			return true
		}
	}
	return false
}

// taken reports whether the store already holds a file under this name.
//
// Derived from the listing rather than carried as its own set, so it cannot go
// stale against it -- and it counts the unusable entries too: a team called
// `platform` occupies the name `platform` however unpickable it is as a member.
func (m Model) taken(name string) bool {
	for _, c := range m.Cands {
		if c.Name == name {
			return true
		}
	}
	return false
}
