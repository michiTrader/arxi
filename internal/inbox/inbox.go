// Package inbox is the human half of a run: the questions an agent asked, and
// the answers that unblock it.
//
// # There is no inbox database, and that is the design
//
// The obvious shape for this package is a store: a directory of question files,
// created when an agent asks and deleted when a human answers. It is not built
// that way, and the reason is the one property this project sells.
//
// A question already exists in the log. `applyToolDenied` mints the id, records
// the item in State, and returns an AskHuman effect; the effect runner appends
// the resulting inbox.created; and kernel.Fold rebuilds State.Inbox from those
// events on every read. So the question ALREADY outlives the process, and it
// does so in the one artefact that is append-only and replayable.
//
// A second copy on disk could only make things worse. Two records of one
// question can disagree, and the moment they do there is no rule for which one
// wins: the file says answered and the log says waiting, or the reverse, and
// `run why` and `arxi inbox` start telling the user different stories about the
// same run. That is precisely the failure ADR-0002 exists to prevent -- if the
// snapshot and the log disagree, the log wins -- and the cheapest way to honour
// it is to have nothing that can disagree.
//
// So this package READS the log and folds it. Listing is a fold. Answering is an
// append. There is no third thing to keep in sync.
//
// # Why the reader does not use logstore.Open
//
// logstore.Open acquires the writer lock and holds it for the lifetime of the
// Store, because seq assignment has to be indivisible. `arxi inbox` is a read
// performed by a human, usually while a run is sitting there blocked -- which is
// exactly when another process owns that lock.
//
// A reader that took the writer lock would therefore fail precisely in the
// situation it was built for, and the error would be about locking rather than
// about the run. So List reads the events file directly and folds them. It
// cannot corrupt anything, because it never writes; and reading a log that is
// being appended to can only miss the newest entries, which for a question a
// human is about to answer is a stale list, not a wrong one.
//
// Answer DOES take the lock, because it appends.
package inbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/michiTrader/arxi/internal/blueprint"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/logstore"
)

// ErrNoSuchItem means the id names no question in this run.
//
// A sentinel, and not a formatted string, because the caller has to tell this
// apart from "the run directory is unreadable". Those have opposite remedies:
// the first is a typo in an id the user copied, the second is a broken run. A
// caller that could only match on wording would eventually match on the wrong
// half of a sentence, which is the defect the toolrun sentinels were introduced
// for.
var ErrNoSuchItem = errors.New("no such inbox item")

// now is the clock, indirected so a test can pin it. The rest of this project
// takes the same seam (nowFunc in cmd/arxi) rather than calling time.Now at the
// point of use, because an assertion on a timestamp produced by the real clock
// can only check that the field is non-empty -- which passes just as well when
// the value is wrong.
var now = time.Now

// ErrAlreadyAnswered means the question was answered before.
//
// Separate from ErrNoSuchItem because answering twice is not a mistake about
// WHICH question, it is a mistake about WHEN -- usually two people looking at
// the same blocked run, or one person running the command twice because the
// first attempt printed nothing. Reporting "no such item" for that would send
// them hunting for an id that is right there in the list.
//
// It is refused rather than tolerated because the second answer is not
// harmless: applyInboxReplied spawns a turn for the member it unblocks, so a
// duplicate reply buys a duplicate turn, and turns cost money.
var ErrAlreadyAnswered = errors.New("inbox item is already answered")

// Item is a question as a human needs to see it, which is the folded InboxItem
// plus the run it belongs to.
//
// RunID is carried because the id an agent's question gets -- inbox-1 -- is
// minted per run and is therefore not unique across runs. §20.2 prints a RUN
// column for exactly that reason: two blocked runs both have an inbox-1, and a
// list that showed only the id would invite approving the wrong one.
type Item struct {
	RunID    string `json:"run_id"`
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Question string `json:"question"`
	Agent    string `json:"agent,omitempty"`
	// OnTimeout is what happens if nobody answers, decided when the question
	// was created. Shown because it is the cost of ignoring the item, and a
	// human deciding whether to deal with this now needs to know whether
	// "later" means denied or means the run fails.
	OnTimeout string `json:"on_timeout,omitempty"`
	Replied   bool   `json:"replied,omitempty"`
}

// Reply is how a question is answered, and the three verbs are kept apart.
//
// §20.2 is explicit that these are different acts: reject "refuses a request and
// carries a reason that reaches the agent as context", while reply "answers a
// question -- there was nothing to authorize". Collapsing them would force the
// agent to guess whether "no" meant NOT ALLOWED or NOT THAT WAY, and those lead
// to opposite next moves: abandon the approach, or retry it differently.
type Reply struct {
	// Decision is "approve", "reject" or "answer".
	Decision string
	// Text is the reason for a rejection or the answer to a question. Empty is
	// legal for an approval, where there is nothing to say beyond yes.
	Text string
}

const (
	DecisionApprove = "approve"
	DecisionReject  = "reject"
	DecisionAnswer  = "answer"
)

// Run is one run directory, opened for reading.
type Run struct {
	dir    string
	runID  string
	config kernel.Config
	state  kernel.State
}

// eventsFileName duplicates logstore's constant deliberately, and the test
// TestTheEventsFileNameAgreesWithLogstore compares them by behaviour: it writes
// a log through logstore and requires this package to read it. A copied name
// that silently drifted would make List return "no questions" for every run,
// which is indistinguishable from a run that has none -- the failure mode this
// package is least able to notice.
const eventsFileName = "events.ndjson"

// OpenRun folds a run directory without taking the writer lock.
//
// The blueprint snapshot is loaded rather than the live blueprint file, because
// Fold needs the Config the events were decided against. Reading the original
// path would explain a run with rules that were never applied -- and after
// somebody edits their blueprint, that is not a hypothetical.
func OpenRun(dir string) (*Run, error) {
	if dir == "" {
		return nil, fmt.Errorf("inbox: no run directory given")
	}

	raw, err := os.ReadFile(filepath.Join(dir, eventsFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("inbox: %s holds no event log (%s), so it is not "+
				"a run directory.\n"+
				"  runs live under ./runs/<run-id> unless --dir said otherwise",
				dir, eventsFileName)
		}
		return nil, fmt.Errorf("inbox: read the log of %s: %w", dir, err)
	}

	cfg, err := loadFrozenConfig(dir)
	if err != nil {
		return nil, err
	}

	events, err := decodeEvents(dir, raw)
	if err != nil {
		return nil, err
	}

	st, _ := kernel.Fold(kernel.State{}, events, cfg)
	return &Run{dir: dir, runID: st.RunID, config: cfg, state: st}, nil
}

// Dir reports the directory this run was read from.
func (r *Run) Dir() string { return r.dir }

// RunID reports the run's id as the log states it.
func (r *Run) RunID() string { return r.runID }

// State returns the folded state, so a caller can explain the run without
// folding it a second time.
func (r *Run) State() kernel.State { return r.state }

// Config returns the frozen config the events were decided against.
func (r *Run) Config() kernel.Config { return r.config }

// List returns the questions of this run.
//
// pending=true filters to the unanswered ones, which is what `arxi inbox`
// shows: a list dominated by history is a list nobody reads, and the whole
// point of the command is that it is short enough to act on.
//
// Answered items are still reachable, because "did I already approve that?" is
// a real question and the log is the only honest answer to it.
func (r *Run) List(pending bool) []Item {
	out := make([]Item, 0, len(r.state.Inbox))
	for _, it := range r.state.Inbox {
		if pending && it.Replied {
			continue
		}
		out = append(out, Item{
			RunID: r.runID, ID: it.ID, Kind: it.Kind, Question: it.Question,
			Agent: it.Agent, OnTimeout: it.OnTimeout, Replied: it.Replied,
		})
	}
	// Ordered by the numeric part of the id, which is the order the questions
	// were asked. State.Inbox is already in that order today; sorting anyway
	// costs nothing and means a future reducer change cannot silently reorder
	// what a human is choosing from by position.
	sort.SliceStable(out, func(i, j int) bool {
		return itemLess(out[i].ID, out[j].ID)
	})
	return out
}

// Item finds one question by id.
func (r *Run) Item(id string) (Item, error) {
	for _, it := range r.List(false) {
		if it.ID == id {
			return it, nil
		}
	}
	return Item{}, fmt.Errorf("inbox: %q in run %s: %w", id, r.runID, ErrNoSuchItem)
}

// Answer appends the reply that unblocks the run.
//
// It takes the writer lock, because this is a write, and it re-folds the log
// under that lock before deciding anything. Checking the freshly-read state and
// not the one OpenRun folded is the difference between refusing a duplicate and
// racing one: between listing and answering, the run may have timed the question
// out or another person may have approved it, and a decision made from the
// stale fold would append a second reply that the reducer honours by spawning a
// second turn.
func Answer(dir string, id string, reply Reply) (kernel.Event, error) {
	if err := validDecision(reply); err != nil {
		return kernel.Event{}, err
	}

	cfg, err := loadFrozenConfig(dir)
	if err != nil {
		return kernel.Event{}, err
	}

	store, err := logstore.Open(dir)
	if err != nil {
		return kernel.Event{}, err
	}
	defer store.Close()

	st, err := store.Fold(cfg, 0)
	if err != nil {
		return kernel.Event{}, err
	}

	found := false
	for _, it := range st.Inbox {
		if it.ID != id {
			continue
		}
		found = true
		if it.Replied {
			return kernel.Event{}, fmt.Errorf("inbox: %q in run %s: %w",
				id, st.RunID, ErrAlreadyAnswered)
		}
	}
	if !found {
		return kernel.Event{}, fmt.Errorf("inbox: %q in run %s: %w",
			id, st.RunID, ErrNoSuchItem)
	}

	ev := kernel.Event{
		ID:   "inbox-reply-" + id,
		Type: kernel.InboxReplied,
		// Ts is set HERE because nothing else will. The effect runner stamps the
		// events IT produces (Runner.stamp fills an empty Ts from its clock),
		// and this append does not go through the effect runner -- a human typed
		// a command, there is no run loop in this process. Measured on a real
		// log before fixing it: the reply landed with "ts":"".
		//
		// An audit record of who authorised a tool, carrying no "when", answers
		// half of the only question anybody asks about a destructive command.
		Ts:     now().UTC().Format(time.RFC3339),
		Source: kernel.SourceHuman,
		Payload: map[string]any{
			"inbox_id": id,
			// "text" is the spec's field name (spec/events.md), and it is what
			// the agent reads as context on its next turn.
			"text": reply.Text,
			// The decision is recorded ALONGSIDE the text, not encoded in it.
			// The reducer unblocks on inbox_id alone and never reads either
			// field, so an approval and a rejection are the same event to it --
			// which means if the distinction is not written down here, the log
			// cannot say afterwards whether a human said yes or no. For a
			// command whose entire purpose is authorising a tool, that is the
			// one fact the history must not lose.
			"decision": reply.Decision,
		},
	}

	// Source is SourceHuman, and it is load-bearing rather than decorative: it
	// is the only thing in the log that distinguishes a human approving a tool
	// from the runtime timing the question out. An audit that cannot tell those
	// apart cannot answer "who allowed this", which is the question anybody
	// looking at a destructive command will ask first.
	written, err := store.Append([]kernel.Event{ev})
	if err != nil {
		return kernel.Event{}, err
	}
	if len(written) != 1 {
		return kernel.Event{}, fmt.Errorf("inbox: appended one reply and the log "+
			"reported %d events written", len(written))
	}
	return written[0], nil
}

// validDecision refuses a reply whose decision is not one of the three.
//
// An unrecognised decision is refused rather than defaulted, because every
// available default is a lie about what a human said. Defaulting to approve
// authorises a command nobody authorised; defaulting to reject discards work on
// the strength of a typo.
func validDecision(r Reply) error {
	switch r.Decision {
	case DecisionApprove, DecisionAnswer:
		return nil
	case DecisionReject:
		// A rejection with no reason is refused, and this is the one place this
		// package is deliberately stricter than it has to be. §20.2 has reject
		// "carry a reason that reaches the agent as context" -- an empty one
		// leaves the agent knowing only that its approach was refused, and its
		// most reasonable next move is to try the same thing again. The reason
		// is not politeness, it is the input to the retry.
		if strings.TrimSpace(r.Text) == "" {
			return fmt.Errorf("inbox: a rejection needs a reason.\n" +
				"  the reason reaches the agent as context for its next turn, and\n" +
				"  without one its best guess is to retry what you just refused")
		}
		return nil
	case "":
		return fmt.Errorf("inbox: no decision given (want %q, %q or %q)",
			DecisionApprove, DecisionReject, DecisionAnswer)
	default:
		return fmt.Errorf("inbox: %q is not a decision (want %q, %q or %q)",
			r.Decision, DecisionApprove, DecisionReject, DecisionAnswer)
	}
}

// loadFrozenConfig reads the blueprint snapshot written when the run started.
func loadFrozenConfig(dir string) (kernel.Config, error) {
	path := filepath.Join(dir, "blueprint.snapshot.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Not fatal. A run started from a state file, and every scenario in
			// testdata, has no frozen blueprint -- and the inbox is still
			// readable without one, because the questions are in the events.
			// What an empty Config loses is the roster, so the reducer cannot
			// resume a member it cannot find. Returning the questions and
			// refusing to invent a Config is the honest half of that.
			return kernel.Config{}, nil
		}
		return kernel.Config{}, fmt.Errorf("inbox: read the frozen blueprint of %s: %w", dir, err)
	}
	bp, err := blueprint.Load(raw)
	if err != nil {
		return kernel.Config{}, fmt.Errorf("inbox: the frozen blueprint of %s does not "+
			"parse, so this run cannot be folded: %w", dir, err)
	}
	return bp.Config, nil
}

// decodeEvents parses the log the same way logstore does, and reports the line.
//
// A gap check is deliberately NOT done here. logstore.Open validates that seq
// runs 1..N and refuses a log with a hole, and duplicating that would give two
// answers to one question. What this reader must not do is pretend a truncated
// last line is a missing question: a log being appended to right now legitimately
// ends mid-line, so an incomplete final line is skipped rather than treated as
// corruption. Refusing there would make `arxi inbox` fail exactly while a run is
// active, which is when it is used.
func decodeEvents(dir string, raw []byte) ([]kernel.Event, error) {
	var out []kernel.Event
	lineNo := 0
	for len(raw) > 0 {
		i := bytes.IndexByte(raw, '\n')
		if i < 0 {
			// No terminating newline: a batch in flight. Stop, keeping what was
			// complete.
			break
		}
		line := raw[:i]
		raw = raw[i+1:]
		lineNo++
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e kernel.Event
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("inbox: %s line %d of the log does not parse: %w\n"+
				"  the log is the run's only history, so this is not skipped",
				dir, lineNo, err)
		}
		out = append(out, e)
	}
	return out, nil
}

// itemLess orders inbox-N ids numerically.
//
// Lexicographic order would put inbox-10 before inbox-2, so the tenth question
// asked would appear second in a list a human picks from by eye. That is a
// cosmetic bug right up until somebody approves the wrong line.
func itemLess(a, b string) bool {
	na, oka := itemNum(a)
	nb, okb := itemNum(b)
	if oka && okb {
		return na < nb
	}
	return a < b
}

func itemNum(id string) (int, bool) {
	const prefix = "inbox-"
	if !strings.HasPrefix(id, prefix) {
		return 0, false
	}
	n := 0
	digits := id[len(prefix):]
	if digits == "" {
		return 0, false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}
