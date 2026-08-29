package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/michiTrader/iash/internal/surface"
	"github.com/michiTrader/iash/internal/trigger"
	"github.com/michiTrader/iash/internal/trigstore"
)

// The `trigger` commands: create, list, show, pause.
//
// # The parser is derived from the declaration, not written out
//
// run start hand-writes a switch over its flags, and that was the right shape
// there — its values need individual coercion and one of them is a positional
// that doubles as a flag. `trigger create` is the opposite case: seven flags,
// four required, two enumerated, and every one of those facts is ALREADY stated
// in the registry. Writing the switch by hand would mean stating them twice, and
// the second copy is the one nobody updates: a flag added to the registry would
// be advertised by `iash surface`, offered to agents in the tool schema, and
// then dropped on the floor here — parsed as unknown or, worse, accepted and
// ignored.
//
// That failure has already happened once in this repository, in the direction
// this file is guarding: `--then` carried a hand-written list of actions
// (`run:|emit:|notify:`) that had gone stale, naming a command that does not
// exist. So the parser reads c.Params: requiredness, defaults and enums are
// enforced from the same declaration that publishes them.
//
// The cost is honest — a generic parser gives worse messages than a bespoke one
// unless it is written to name the parameter and the legal set every time, which
// is why parseInvocation does exactly that.

// nowFunc is the clock. It is a variable so tests can pin it: every printed
// NEXT column is derived from it, and a test that could only compare against
// whenever the suite happened to run would have to assert nothing.
var nowFunc = func() time.Time { return time.Now().UTC() }

// triggerDir is where triggers live. A variable so tests do not write into the
// developer's working directory.
var triggerDir = trigstore.DefaultDir

func cmdTrigger(args []string) {
	if len(args) == 0 {
		triggerUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		cmdTriggerCreate(args[1:])
	case "list":
		cmdTriggerList(args[1:])
	case "show":
		cmdTriggerShow(args[1:])
	case "pause":
		cmdTriggerPause(args[1:])
	default:
		// `trigger delete` is the specific guess worth answering properly: it
		// is what a user reaches for, it is declared nowhere, and "unknown
		// subcommand" would leave them believing they typed it wrong.
		if args[0] == "delete" || args[0] == "rm" || args[0] == "remove" {
			fmt.Fprintln(os.Stderr, "iash trigger has no delete.\n"+
				"  use `iash trigger pause NAME`: silencing a noisy trigger while "+
				"you investigate should not destroy its configuration and its "+
				"history, and the reason it was stopped is usually the thing you "+
				"want to read later (docs/design/20-use-cases.md §20.10)")
			os.Exit(2)
		}
		// A trigger subcommand that IS declared but has no implementation here
		// must fall through to main's "declared but not implemented" answer,
		// not be called unknown. Saying "not a trigger command" about something
		// `iash surface` lists sends the user hunting for a typo they never
		// made — the precise failure main.go's fallback exists to prevent, and
		// which this switch would reintroduce for whatever is declared next.
		if surface.Lookup("trigger", args[0]) != nil {
			notImplemented(append([]string{"trigger"}, args...))
		}
		fmt.Fprintf(os.Stderr, "iash trigger: %q is not a trigger command.\n", args[0])
		triggerUsage()
		os.Exit(2)
	}
}

func triggerUsage() {
	fmt.Fprint(os.Stderr, `usage:
  iash trigger create NAME --on SPEC --then CMD --budget N --budget-period P
  iash trigger list [--json]
  iash trigger show NAME [--json]
  iash trigger pause NAME

  iash trigger create --help    every flag, with its legal values
`)
}

// openStore prepares the trigger directory.
func openStore() *trigstore.Store {
	s, err := trigstore.Open(triggerDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash trigger: %v\n", err)
		os.Exit(1)
	}
	return s
}

func cmdTriggerCreate(args []string) {
	c := surface.Lookup("trigger", "create")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash trigger create: %v\n", err)
		os.Exit(2)
	}

	budget, err := strconv.ParseFloat(vals["budget"], 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash trigger create: --budget %q is not a "+
			"number.\n  it is a spend ceiling in whole currency units, e.g. "+
			"--budget 5.00\n", vals["budget"])
		os.Exit(2)
	}

	now := nowFunc()
	r := trigger.Record{
		Name:         vals["name"],
		On:           vals["on"],
		Then:         vals["then"],
		Budget:       budget,
		BudgetPeriod: trigger.Period(vals["budget-period"]),
		OnMissed:     trigger.OnMissed(vals["on-missed"]),
		Overlap:      trigger.Overlap(vals["overlap"]),
		Status:       trigger.StatusActive,
		CreatedAt:    now.Format(time.RFC3339),
	}

	// Validate before touching the disk. The store validates too, so this is
	// not the only guard — but it is what makes a mistyped cron field a USAGE
	// error (exit 2) instead of an operational failure (exit 1). Without it the
	// same typo arrives as "the trigger store refused this", and a CI job that
	// separates the two would file a broken-storage report for a bad schedule.
	if err := r.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "iash trigger create: %v\n", err)
		os.Exit(2)
	}

	if err := openStore().Create(r); err != nil {
		fmt.Fprintf(os.Stderr, "iash trigger create: %v\n", err)
		os.Exit(1)
	}

	// §20.10 prints the next firing on create, and it is the most useful thing
	// on the line: it is the only feedback that says the schedule means what
	// the user thought. `cron:0 3 * * 0` looks daily until it answers with a
	// Sunday.
	fmt.Printf("trigger %s created (next: %s)\n", r.Name, nextColumn(r, now))
}

func cmdTriggerList(args []string) {
	c := surface.Lookup("trigger", "list")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash trigger list: %v\n", err)
		os.Exit(2)
	}

	rs, err := openStore().List()
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash trigger list: %v\n", err)
		os.Exit(1)
	}
	now := nowFunc()

	if vals["json"] == "true" {
		emitJSON(listPayload(rs, now))
		return
	}

	// An empty list says so. A bare header row is the output that makes a user
	// wonder whether the command worked, and the answer they need — that
	// nothing is scheduled — is precisely what a blank table fails to state.
	if len(rs) == 0 {
		fmt.Printf("no triggers in %s/\n", triggerDir)
		fmt.Println("  create one: iash trigger create NAME --on \"cron:0 3 * * *\" " +
			"--then \"run start team 'objective'\" --budget 5.00 --budget-period day")
		return
	}

	rows := [][5]string{{"NAME", "ON", "STATUS", "LAST", "NEXT"}}
	for _, r := range rs {
		rows = append(rows, [5]string{r.Name, r.On, string(r.Status),
			lastColumn(r), nextColumn(r, now)})
	}
	// Widths are measured, not fixed. §20.10's example is aligned for one
	// trigger called nightly-audit; a hardcoded %-16s turns into a ragged table
	// the first time somebody uses a longer name, and the column a reader scans
	// down is the one that stops lining up.
	var w [5]int
	for _, row := range rows {
		for i, cell := range row {
			if len(cell) > w[i] {
				w[i] = len(cell)
			}
		}
	}
	for _, row := range rows {
		var b strings.Builder
		for i, cell := range row {
			if i == len(row)-1 {
				b.WriteString(cell) // no trailing padding on the last column
				break
			}
			fmt.Fprintf(&b, "%-*s  ", w[i], cell)
		}
		fmt.Println(b.String())
	}
}

func cmdTriggerShow(args []string) {
	c := surface.Lookup("trigger", "show")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash trigger show: %v\n", err)
		os.Exit(2)
	}

	r, err := openStore().Load(vals["name"])
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash trigger show: %v\n", err)
		os.Exit(1)
	}
	now := nowFunc()

	if vals["json"] == "true" {
		emitJSON(showPayload(r, now))
		return
	}

	fmt.Printf("trigger %s\n", r.Name)
	fmt.Printf("  on:            %s\n", r.On)
	fmt.Printf("  then:          %s\n", r.Then)
	fmt.Printf("  budget:        %.2f per %s\n", r.Budget, r.BudgetPeriod)
	fmt.Printf("  on-missed:     %s\n", r.OnMissed)
	fmt.Printf("  overlap:       %s\n", r.Overlap)
	fmt.Printf("  status:        %s\n", r.Status)
	fmt.Printf("  created:       %s\n", r.CreatedAt)
	fmt.Printf("  last fired:    %s\n", lastFiredLine(r))
	fmt.Printf("  next:          %s\n", nextColumn(r, now))

	// The counter-intuitive rule is announced where the schedule is displayed,
	// not left in the documentation. When both day fields are restricted cron
	// fires when EITHER matches, so `0 3 1 * 1` is the 1st of the month AND
	// every Monday — read as an AND it looks like a once-a-month job, and the
	// difference is four extra runs a month that nobody authorised.
	if s, err := r.Spec(); err == nil && s.AmbiguousDayRule() {
		fmt.Println("\n  note: day-of-month and day-of-week are both restricted, " +
			"so this fires when EITHER\n        matches — that is cron's rule, " +
			"and it fires more often than reading it as AND suggests.")
	}

	// A trigger that has slept through firings should say so here, because this
	// is the command a user runs when they suspect it has. What happens to
	// those firings is --on-missed's decision, so both are printed together:
	// "4 missed" with no policy beside it invites the assumption that they are
	// queued.
	if n, capped, err := r.Missed(now); err == nil && n > 0 {
		count := strconv.Itoa(n)
		if capped {
			count = "at least " + count
		}
		fmt.Printf("\n  %s scheduled firing(s) were missed; on-missed=%s decides "+
			"what happens to them\n", count, r.OnMissed)
	}
}

func cmdTriggerPause(args []string) {
	c := surface.Lookup("trigger", "pause")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash trigger pause: %v\n", err)
		os.Exit(2)
	}

	st := openStore()
	r, err := st.Load(vals["name"])
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash trigger pause: %v\n", err)
		os.Exit(1)
	}

	// Pausing an already-paused trigger reports it rather than succeeding
	// silently. Silence here reads as "I have just stopped it", which is the
	// wrong belief to hold about a trigger somebody else paused for a reason.
	if r.Status == trigger.StatusPaused {
		fmt.Printf("trigger %s was already paused (since %s)\n", r.Name,
			pausedSince(r))
		return
	}

	r.Status = trigger.StatusPaused
	if err := st.Save(r); err != nil {
		fmt.Fprintf(os.Stderr, "iash trigger pause: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("trigger %s paused (it will not fire; resume is not implemented yet)\n",
		r.Name)
}

// nextColumn is the NEXT cell, and every way of having no answer is a different
// sentence.
//
// A blank cell would collapse four unrelated situations — paused, external,
// exhausted, broken — into one, and three of them are things the user needs to
// act on. This is the same argument as `why`: the point of the column is to
// explain, and "-" explains nothing.
func nextColumn(r trigger.Record, now time.Time) string {
	t, ok, err := r.Next(now)
	switch {
	case err != nil:
		return "unresolvable"
	case ok:
		// The format §20.10 prints. Z rather than +00:00 because it is shorter
		// and unambiguous, and UTC rather than local because a trigger that
		// reports a local time fires at a different printed hour after a DST
		// change while the schedule has not moved.
		return t.UTC().Format("2006-01-02 15:04Z")
	case r.Status == trigger.StatusPaused:
		return "(paused)"
	default:
		return "(on event)"
	}
}

func lastColumn(r trigger.Record) string {
	if r.LastFiredAt == "" {
		// "never" and not "-": a trigger created a month ago that has never
		// fired is the exact situation this column exists to reveal.
		return "never"
	}
	if r.LastStatus == "" {
		return "fired"
	}
	return r.LastStatus
}

func lastFiredLine(r trigger.Record) string {
	if r.LastFiredAt == "" {
		return "never"
	}
	if r.LastStatus == "" {
		return r.LastFiredAt
	}
	return fmt.Sprintf("%s (%s)", r.LastFiredAt, r.LastStatus)
}

// pausedSince reports what is actually known. The record does not store when it
// was paused, and inventing a timestamp — CreatedAt, or now — would be a fact
// the file does not contain, printed in the position a reader trusts most.
func pausedSince(r trigger.Record) string {
	if r.LastFiredAt != "" {
		return "last fired " + r.LastFiredAt
	}
	return "it has never fired"
}

// triggerJSON is the --json shape.
//
// Derived values are included and clearly separated from stored ones. `next` is
// computed on every read and never written (ADR-0002): a stored one rots by
// existing, because after four days of downtime it is a past timestamp and
// indistinguishable from a broken schedule. But a machine reading this output
// needs it, and making every caller reimplement the cron walk to get it is how
// two answers to the same question appear.
//
// `next` is RFC3339 or absent — never the human string the table prints. The
// table's "(paused)" and "(on event)" are sentences for a person; in a JSON
// field named next they are a parse failure waiting for whoever assumed a
// timestamp, and the ones who do not crash will compare them as strings and
// silently treat a paused trigger as scheduled. When there is no next firing
// the field is omitted and `next_absent` says which of the reasons it is, so a
// machine must handle the absence explicitly rather than misreading text.
type triggerJSON struct {
	Record     trigger.Record `json:"record"`
	Next       string         `json:"next,omitempty"`
	NextAbsent string         `json:"next_absent,omitempty"`
	Note       string         `json:"note,omitempty"`
	Missed     int            `json:"missed,omitempty"`
}

func showPayload(r trigger.Record, now time.Time) triggerJSON {
	out := triggerJSON{Record: r}
	t, ok, err := r.Next(now)
	switch {
	case err != nil:
		out.NextAbsent = "unresolvable"
	case ok:
		out.Next = t.UTC().Format(time.RFC3339)
	case r.Status == trigger.StatusPaused:
		out.NextAbsent = "paused"
	default:
		out.NextAbsent = "external"
	}
	if s, err := r.Spec(); err == nil && s.AmbiguousDayRule() {
		out.Note = "day-of-month and day-of-week are both restricted: cron fires " +
			"when either matches, not both"
	}
	if n, _, err := r.Missed(now); err == nil {
		out.Missed = n
	}
	return out
}

func listPayload(rs []trigger.Record, now time.Time) map[string]any {
	out := make([]triggerJSON, 0, len(rs))
	for _, r := range rs {
		out = append(out, showPayload(r, now))
	}
	// A named key rather than a bare array, so a later addition (a count, a
	// warning about an unreadable file) does not change the type of the whole
	// document and break every parser at once.
	return map[string]any{"triggers": out}
}

func emitJSON(v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "iash trigger: encoding JSON: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(b))
}

// parseInvocation parses args against a command's DECLARED parameters.
//
// Requiredness, defaults, enums and positionals all come from the registry, so
// this function has no list of its own to fall out of date. Short flags are
// expanded first by the shared expander, for the same reason: one letter means
// one thing across the whole surface.
//
// Names are returned with dashes as declared (`budget-period`), not with the
// underscores WireParams uses for the wire, because these are the CLI spellings
// and the caller above reads them next to the flags a user types.
func parseInvocation(c *surface.Cmd, args []string) (map[string]string, error) {
	if c == nil {
		return nil, fmt.Errorf("this command is not in the surface (internal error)")
	}
	for _, a := range args {
		if a == "--help" || a == "-h" {
			printDeclaredHelp(c)
			os.Exit(0)
		}
	}

	args, err := expandShort(c, args)
	if err != nil {
		return nil, err
	}

	vals := map[string]string{}
	var positionals []string

	// Long-flag names, from the declaration. `--json` is included for reading
	// commands because WireParams synthesises it there, so a command that
	// advertises machine-readable output cannot fail to accept the flag.
	declared := map[string]surface.Param{}
	for _, pp := range c.Params {
		declared[pp.Name] = pp
	}
	if !c.Mutates {
		declared["json"] = surface.Param{Name: "json", Type: "bool", Desc: "JSON output"}
	}

	for i := 0; i < len(args); i++ {
		a := args[i]

		if a == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "--") {
			positionals = append(positionals, a)
			continue
		}

		name, inlineVal, inline := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		pp, ok := declared[name]
		if !ok {
			return nil, unknownFlag(c, name)
		}

		if pp.Type == "bool" {
			if inline {
				vals[name] = inlineVal
				continue
			}
			vals[name] = "true"
			continue
		}

		if inline {
			vals[name] = inlineVal
		} else {
			if i+1 >= len(args) {
				// The value is named, and so is what it is for. "--on needs a
				// value" alone leaves the user guessing at the syntax of the
				// thing they just failed to supply.
				return nil, fmt.Errorf("--%s needs a value (%s)", name, pp.Desc)
			}
			i++
			vals[name] = args[i]
		}
	}

	// Positionals are assigned in declared order. A positional given twice —
	// once by position and once as --name — is refused rather than resolved by
	// precedence: whichever one loses is a value the user supplied and watched
	// disappear.
	var wantPos []surface.Param
	for _, pp := range c.Params {
		if pp.Positional {
			wantPos = append(wantPos, pp)
		}
	}
	if len(positionals) > len(wantPos) {
		return nil, fmt.Errorf("too many arguments: %s takes %d (%s), and got %d (%s)",
			c.CLI(), len(wantPos), positionalNames(wantPos),
			len(positionals), strings.Join(positionals, " "))
	}
	for i, v := range positionals {
		n := wantPos[i].Name
		if prev, dup := vals[n]; dup {
			return nil, fmt.Errorf("%s was given twice: --%s %q and the "+
				"positional %q. Pick one — dropping either silently would "+
				"discard a value you typed", n, n, prev, v)
		}
		vals[n] = v
	}

	// Defaults, then requiredness, then enums. In that order, because a default
	// satisfies requiredness and must itself be legal — the surface had a
	// default outside its own enum until this session, and checking enums last
	// is what would have caught it at runtime too.
	for _, pp := range c.Params {
		if _, set := vals[pp.Name]; !set && pp.Default != "" {
			vals[pp.Name] = pp.Default
		}
	}
	var missing []surface.Param
	for _, pp := range c.Params {
		if _, set := vals[pp.Name]; !set && pp.Required {
			missing = append(missing, pp)
		}
	}
	if len(missing) > 0 {
		return nil, missingFlags(c, missing)
	}
	for _, pp := range c.Params {
		v, set := vals[pp.Name]
		if !set || len(pp.Enum) == 0 {
			continue
		}
		if !contains(pp.Enum, v) {
			return nil, fmt.Errorf("--%s %q is not one of %s.\n  %s",
				pp.Name, v, strings.Join(pp.Enum, ", "), pp.Desc)
		}
	}
	return vals, nil
}

func positionalNames(ps []surface.Param) string {
	out := make([]string, 0, len(ps))
	for _, pp := range ps {
		out = append(out, pp.Name)
	}
	return strings.Join(out, ", ")
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// missingFlags names every missing parameter at once, with the reason each one
// is mandatory taken from its own description.
//
// One at a time would make the user re-run the command four times to discover
// four required flags, and `trigger create` has four on purpose — each covers a
// way unattended automation becomes expensive (§20.10).
func missingFlags(c *surface.Cmd, missing []surface.Param) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s needs %d more flag(s):\n", c.CLI(), len(missing))
	for _, pp := range missing {
		if pp.Positional {
			fmt.Fprintf(&b, "  %-16s %s (given by position)\n", pp.Name, pp.Desc)
			continue
		}
		line := fmt.Sprintf("  --%-14s %s", pp.Name, pp.Desc)
		if len(pp.Enum) > 0 {
			line += " [" + strings.Join(pp.Enum, "|") + "]"
		}
		if s := surface.Short(pp.Name); s != "" {
			line += fmt.Sprintf(" (short -%s)", s)
		}
		fmt.Fprintln(&b, line)
	}
	// The reason, not just the requirement. Four mandatory flags is unusual for
	// this surface and looks like bureaucracy until it is said out loud that
	// each one is a way an unwatched job becomes expensive.
	if c.CLI() == "trigger create" {
		b.WriteString("\n  these are mandatory because nobody is watching: a " +
			"per-invocation ceiling\n  is not a ceiling for something that fires " +
			"365 times a year, and the period\n  is what makes the number mean " +
			"anything (docs/design/20-use-cases.md §20.10)")
	}
	return fmt.Errorf("%s", b.String())
}

// unknownFlag refuses rather than ignoring, and looks for the near miss.
//
// A flag that parses and is then dropped is the failure this whole file is
// arranged against: `--budget-preiod day` silently ignored leaves the trigger
// with no period, and the command reports success.
func unknownFlag(c *surface.Cmd, name string) error {
	var known []string
	for _, pp := range c.Params {
		known = append(known, "--"+pp.Name)
	}
	if !c.Mutates {
		known = append(known, "--json")
	}
	sort.Strings(known)

	var b strings.Builder
	fmt.Fprintf(&b, "--%s is not a flag of %s.\n", name, c.CLI())
	// A misspelling is the overwhelmingly likely cause, and the guess is worth
	// more than the list: `--budget-preiod` next to `--budget-period` is
	// recognised instantly, while a nine-item list has to be read.
	if best := nearest(name, known); best != "" {
		fmt.Fprintf(&b, "  did you mean %s?\n", best)
	}
	fmt.Fprintf(&b, "  accepted: %s", strings.Join(known, " "))
	return fmt.Errorf("%s", b.String())
}

// nearest finds a flag within a small edit distance of what was typed.
func nearest(name string, known []string) string {
	best, bestD := "", 3 // further than two edits is not a typo, it is a guess
	for _, k := range known {
		if d := editDistance(name, strings.TrimPrefix(k, "--")); d < bestD {
			best, bestD = k, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = minInt(minInt(cur[j-1]+1, prev[j]+1), prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// printDeclaredHelp prints help built from the declaration.
//
// Help written by hand is documentation that drifts from the parser beside it,
// and help is exactly where drift is most expensive: it is what a user reads
// INSTEAD of the source. Built from c.Params, it cannot describe a flag the
// parser does not accept, or omit one it does.
func printDeclaredHelp(c *surface.Cmd) {
	fmt.Printf("iash %s — %s\n\n", c.CLI(), c.Desc)
	for _, pp := range c.Params {
		kind := "  --" + pp.Name
		if pp.Positional {
			kind = "  " + pp.Name
		}
		if s := surface.Short(pp.Name); s != "" && !pp.Positional {
			kind += ", -" + s
		}
		fmt.Printf("%-26s %s\n", kind, pp.Desc)
		if len(pp.Enum) > 0 {
			fmt.Printf("%-26s   one of: %s\n", "", strings.Join(pp.Enum, ", "))
		}
		switch {
		case pp.Default != "":
			fmt.Printf("%-26s   default: %s\n", "", pp.Default)
		case pp.Required:
			fmt.Printf("%-26s   required\n", "")
		}
	}
	if !c.Mutates {
		fmt.Printf("%-26s %s\n", "  --json, -J", "JSON output")
	}
	fmt.Printf("\ntool name: %s · protocol: %s · since surface v%d\n",
		c.Name(), c.ProtocolType(), c.Since)
}
