package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/michiTrader/arxi/internal/agentstore"
	"github.com/michiTrader/arxi/internal/blueprint"
	"github.com/michiTrader/arxi/internal/kernel"
	"github.com/michiTrader/arxi/internal/surface"
)

const blueprintUsage = "usage: arxi blueprint create <name> --members a,b,c [--stages s1,s2]\n" +
	"       arxi blueprint install <path|https URL> [--as <name>]\n" +
	"       arxi blueprint validate <file.yaml>\n" +
	"  create composes agents already in " + agentstore.DefaultDir + "/ into one file of the\n" +
	"  same kind, in the same directory: `arxi agent list` shows it and\n" +
	"  `arxi run start <name>` runs it. members are COPIED, not referenced, so\n" +
	"  editing an agent afterwards does not change a team already composed.\n" +
	"  without --stages there is one stage named work, and every member is in it.\n" +
	"  install puts somebody else's blueprint in the same directory, byte for byte,\n" +
	"  under its own `name:` unless --as says otherwise. read it before you run it:\n" +
	"  it is a file that names tools and a budget is what runs it.\n"

// cmdBlueprintCreate implements `arxi blueprint create <name> --members a,b,c`.
//
// It composes agents that already exist rather than asking for members on the
// command line, and that is the whole design: a member is a name, a model, a role,
// a tool grant, an activation mode and an advisory flag, and a flag syntax able to
// carry six fields for each of three members is a file format with dashes in it.
// `agent create` already writes those files one at a time and `agent list` already
// shows them, so the composition step has nothing left to ask.
//
// Members are COPIED into the new file, and internal/agentstore's package doc
// argues that at length: `run start` freezes runs/<id>/blueprint.snapshot.yaml and
// hashes it (ADR-0001, ADR-0002), so a `members: [ref: backend]` would leave part
// of the run's rules outside the snapshot and outside the SHA. Editing an agent
// would then change what a team composed months earlier does, and the frozen file
// would not show it. The copy is also what makes the refusals below necessary:
// nothing revisits agents/backend.yaml to notice that its `stages: [build]` names
// no stage of the team it was copied into.
func cmdBlueprintCreate(args []string) {
	c := surface.Lookup("blueprint", "create")
	vals, err := parseInvocation(c, args)
	if err != nil {
		// parseInvocation enforces `--members` from the declaration, so a missing
		// one arrives here already naming the flag and quoting its description.
		// cmd/arxi holds no list of its own, which is what keeps the CLI refusal and
		// the tool-schema refusal the same refusal.
		fmt.Fprintf(os.Stderr, "arxi blueprint create: %v\n\n%s", err, blueprintUsage)
		os.Exit(2)
	}

	names := splitCSV(vals["members"])
	if len(names) == 0 {
		// Reachable past a satisfied `--members`: the flag was given and its value
		// was `,` or a space. Requiredness is about presence, and splitCSV drops the
		// empty entries that presence let through.
		fmt.Fprintf(os.Stderr, "arxi blueprint create: --members is empty\n"+
			"  a blueprint with no members enters its first stage, activates nobody\n"+
			"  and goes quiescent after zero turns.\n\n%s", blueprintUsage)
		os.Exit(2)
	}

	members, from := resolveMembers(names)
	t := agentstore.Team{
		Name:    vals["name"],
		Members: members,
		Stages:  splitCSV(vals["stages"]),
	}
	// Validated before the store is opened, following `agent create`: a member that
	// could never take a turn is the invocation being wrong (exit 2), and no
	// directory should be created for a command that cannot succeed.
	if err := t.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "arxi blueprint create: %v\n", err)
		printMemberSources(t.Members, from)
		os.Exit(2)
	}

	path, err := openAgents().CreateTeam(t)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi blueprint create: %v\n", err)
		if errors.Is(err, agentstore.ErrExists) {
			// The same refusal `agent create` gives, and it has to be, because the
			// two verbs share one directory: the name may belong to an agent
			// somebody grew by hand, and this command did not touch it.
			fmt.Fprintf(os.Stderr, "  nothing was written. read the one that is there: "+
				"arxi agent show %s\n  or choose another name -- an agent and a team "+
				"compete for the same file.\n", t.Name)
			os.Exit(2)
		}
		os.Exit(1)
	}
	printTeamCreated(t, path, from)
}

// resolveMembers turns the `--members` names into members copied out of the store.
//
// Every failure here exits, and each one prints what the user can do next, because
// the alternatives are all worse than stopping: composing the members that DID
// resolve would write a team smaller than the command line, and creating a
// placeholder for one that did not would write a member with no tools that takes
// paid turns and can do nothing.
//
// The returned map is the source file of each member, keyed by member name. It is
// carried rather than recomputed because it cannot be read back off the finished
// team -- a copied member records nothing about where it came from, which is the
// point of copying -- and it is what lets a refusal name a file the user did not
// type: `--members backend` fails over a `stages:` line in agents/backend.yaml.
func resolveMembers(names []string) ([]kernel.MemberConfig, map[string]string) {
	st := readAgents()
	var out []kernel.MemberConfig
	from := map[string]string{}
	seen := map[string]string{}

	for _, name := range names {
		bp, err := st.Load(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "arxi blueprint create: member %q: %v\n", name, err)
			if errors.Is(err, agentstore.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "  what is stored: arxi agent list\n"+
					"  or write it:    arxi agent create %s --model <id> --tools read\n"+
					"  nothing was written -- a team is composed from agents that exist.\n", name)
			} else {
				// A file that is there and does not load is not a missing agent, for
				// the reason `agent show` gives: sending somebody to `agent create`
				// for a file they can see makes them write one they already have.
				fmt.Fprintf(os.Stderr, "  it did not load, so it cannot be copied. an agent is an\n"+
					"  ordinary blueprint: arxi blueprint validate %s reports the same thing.\n",
					st.Path(name))
			}
			os.Exit(1)
		}

		ms := bp.Config.Members
		if len(ms) == 0 {
			// blueprint.Load accepts a memberless file on purpose, so this one is
			// valid, listed by `agent list`, and has nobody in it to copy.
			fmt.Fprintf(os.Stderr, "arxi blueprint create: member %q has no members of its own\n"+
				"  %s loads and is empty, so there is nothing to copy out of it.\n"+
				"  read it: arxi agent show %s\n", name, st.Path(name), name)
			os.Exit(1)
		}
		if len(ms) > 1 {
			// A team is not a member. Splicing its members in would compose more
			// members than the command line names -- `--members platform,security`
			// silently becoming four -- and would drop the stages that made that
			// file a team, which are the part of it that says who works when. A run
			// that needs a team inside a team is what `run spawn` and sub-runs are
			// for; this verb composes agents.
			var who []string
			for _, m := range ms {
				who = append(who, m.Name)
			}
			fmt.Fprintf(os.Stderr, "arxi blueprint create: member %q is itself a team of %d (%s)\n"+
				"  copying it would compose %d members where the command line names one,\n"+
				"  and would drop the stages that decide which of them works when.\n"+
				"  name its members instead: --members %s\n",
				name, len(ms), strings.Join(who, ", "), len(ms), strings.Join(who, ","))
			os.Exit(2)
		}

		m := ms[0]
		// The MEMBER's name is what carries over, not the file's. Everything that
		// addresses a member downstream uses it -- `run steer <run> <member>`,
		// `agent tool policy --agent <member>`, a watcher's `agent:` key -- so
		// renaming it to the filename here would compose a team whose members
		// answer to names the file it came from does not use. `agent create` writes
		// the two equal; a hand edit can part them, and `agent show` prints the
		// difference for exactly this reason.
		if prev, dup := seen[m.Name]; dup {
			fmt.Fprintf(os.Stderr, "arxi blueprint create: two members would be named %q\n"+
				"  %s and %s\n"+
				"  a member's name is how a stage activates it and how `run steer` "+
				"addresses it,\n  so two of them is a team the kernel cannot address. "+
				"the name inside a file\n  need not match the filename -- arxi agent show "+
				"%s says which one it uses.\n",
				m.Name, prev, st.Path(name), name)
			os.Exit(2)
		}
		seen[m.Name] = st.Path(name)
		from[m.Name] = st.Path(name)
		out = append(out, m)
	}
	return out, from
}

// printMemberSources lists where each member came from, under a refusal.
//
// Only under a refusal, and only because of which refusal it is. Team.Validate's
// message names a member and the stage list that does not match, and neither the
// member name nor that stage name has to appear on the command line the user
// typed: `--members backend --stages review` fails over a `stages: [build]` line
// in a file they are not looking at. The successful case prints the same paths
// through printTeamCreated, where they are part of the report rather than an aid
// to finding something.
func printMemberSources(ms []kernel.MemberConfig, from map[string]string) {
	if len(from) == 0 {
		return
	}
	fmt.Fprint(os.Stderr, "  nothing was written. the members were copied from:\n")
	for _, m := range ms {
		if p := from[m.Name]; p != "" {
			fmt.Fprintf(os.Stderr, "    %s: %s\n", m.Name, p)
		}
	}
}

// printTeamCreated says what was composed, out of what, and what to do next.
//
// Each member's resolved tool policy is on its line for `agent create`'s reason:
// `--tools read,write` looks like two equal grants and `write` resolves to ask, so
// a run stops for an approval nobody expected. A team multiplies that -- three
// members can carry three different grants written on three different days -- and
// the file the operator is about to run is the first place all of them appear
// together.
//
// The source path is printed per member because it is the only thing this screen
// knows that the new file does not record. A copied member is indistinguishable
// from one typed by hand, and the sentence that makes the copy comprehensible --
// editing agents/backend.yaml will not change this team -- is only meaningful next
// to the path it will not change.
func printTeamCreated(t agentstore.Team, path string, from map[string]string) {
	fmt.Printf("blueprint %s created: a team of %d\n", t.Name, len(t.Members))
	fmt.Printf("  file:   %s\n", path)

	for _, m := range t.Members {
		fmt.Printf("  - %s: %s\n", m.Name, grantSummary(m.Tools, overridesFor(m.Name)))
		detail := "advisory"
		if !m.Advisory {
			detail = "counts toward advance"
		}
		if m.Role != "" {
			detail = m.Role + ", " + detail
		}
		fmt.Printf("      %s, %s\n", dash(m.Model), detail)
		if p := from[m.Name]; p != "" {
			fmt.Printf("      copied from %s\n", p)
		}
	}

	// Stage names are read off the Team rather than the file, and the default is
	// spelled out rather than left implied: `--stages` was not given, so `work` is
	// a name the user has not seen, and it is the name `run show` will print and
	// the one a hand-edited `stages:` list on a member has to match.
	stages := t.Stages
	if len(stages) == 0 {
		stages = []string{"work"}
		fmt.Printf("  stages: %s  (the default -- no --stages, so one stage and "+
			"everybody in it)\n", strings.Join(stages, " -> "))
	} else {
		fmt.Printf("  stages: %s  (in order, each advancing when every member "+
			"has submitted)\n", strings.Join(stages, " -> "))
	}

	printTeamCaveats(t.Name, t.Members, stages)
	fmt.Print("  edit it: it is an ordinary blueprint -- add a watcher, a timeout, or\n" +
		"           advance_when: quorum:2, then arxi blueprint validate " + path + "\n")
}

// printTeamCaveats prints what this file will do that its flags do not say.
//
// Two of them, and both are the same kind of failure: a run that starts, reports
// nothing wrong, and does not do the work.
//
// It takes a name, members and stages rather than an agentstore.Team because
// `blueprint install` reports on a file it did not compose: what it holds is a
// kernel.Config off the loader, and there is no Team behind it -- a Team cannot
// carry the watchers and timeouts an installed blueprint is likely to have. The
// caveats are properties of the members and the stage list, so nothing here needed
// the Team; taking one meant install would have had to fabricate one, or copy these
// three warnings. Copying them is the failure mode worth avoiding: they are the
// money warnings, and the installed file is the one nobody in this process wrote.
//
// stages must be non-empty. Both callers guarantee it and neither can do so by
// construction -- ResolveDefaults synthesises no stage, so an installed blueprint
// can genuinely have none, and install says so itself before calling this.
func printTeamCaveats(name string, members []kernel.MemberConfig, stages []string) {
	// A team of only advisory members is the expensive one, and it is the reason
	// this caveat exists at all. applyStageEntered skips advisory members
	// (internal/kernel/decide.go: `if m.Advisory || !participates(...)`), so the
	// run enters the first stage, opens no turn, and records run.quiescent after
	// zero turns -- the same dead end `agent create --advisory` prints, arrived at
	// with three names on the command line instead of one flag.
	var workers, advisory []string
	for _, m := range members {
		if m.Advisory {
			advisory = append(advisory, m.Name)
			continue
		}
		workers = append(workers, m.Name)
	}
	if len(workers) == 0 {
		fmt.Printf("  caveat: every member is advisory, so `arxi run start %s` enters "+
			"%s,\n          activates nobody, and goes quiescent after zero turns.\n"+
			"          add a member that works, or edit `advisory: true` out of one.\n",
			name, stages[0])
		return
	}
	if len(advisory) > 0 {
		// Said even though it is not a defect: an advisory member neither takes a
		// turn nor counts toward the advance rule (quorumMet skips it), so a team
		// of two implementers and one reviewer advances on two submissions. Read
		// off the member list, `advance_when: all` looks like it waits for three.
		fmt.Printf("  note:   %s %s advisory: no turn at stage entry and no vote in the\n"+
			"          advance rule, so each stage advances when %s %s submitted.\n",
			strings.Join(advisory, ", "), isAre(len(advisory)),
			strings.Join(workers, " and "), haveHas(len(workers)))
	}

	// The model note is per member, because a team can be half configured: the
	// agents were created on different days and one `--model` on the run command
	// line covers all of them, so naming the ones that need it is the difference
	// between a fix and a guess.
	var modelless []string
	for _, m := range members {
		if m.Model == "" {
			modelless = append(modelless, m.Name)
		}
	}
	if len(modelless) > 0 {
		fmt.Printf("  note:   no model for %s, so a live run needs --model on the "+
			"command line\n          (it applies to every member). add `model:` to the "+
			"file to stop repeating it.\n", strings.Join(modelless, ", "))
	}

	next := fmt.Sprintf("arxi run start %s \"<objective>\" --budget 5.00", name)
	if len(modelless) > 0 {
		next += " --model <id>"
	}
	fmt.Printf("  run it: %s\n", next)
}

// haveHas agrees the caveat's verb with the number of members it names.
//
// A sibling of event.go's isAre, which this file's advisory note also uses. Three
// lines for grammar, because the alternative is "backend has submitted" beside
// "backend and frontend has submitted" in the output of a command whose entire job
// on that line is to be read carefully.
func haveHas(n int) string {
	if n == 1 {
		return "has"
	}
	return "have"
}

// cmdBlueprintValidate implements `arxi blueprint validate <path>`.
//
// It prints the RESOLVED config, not the file read back. Most of what it prints
// the user never wrote: the workspace, the timeout policy, the activation mode.
// Those defaults are security and cost decisions, and a default you cannot see
// is indistinguishable from a bug when it fires — which is the whole argument of
// docs/design/20-use-cases.md §20.4.
//
// It also explains WHY a resolved value came out the way it did (which members
// forced the worktree). Printing `workspace: worktree` alone invites the user to
// override it as noise; printing who forced it makes the decision reviewable.
func cmdBlueprintValidate(args []string) {
	args, err := expandShort(surface.Lookup("blueprint", "validate"), args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi blueprint validate: %v\n", err)
		os.Exit(2)
	}

	var path string
	for _, a := range args {
		// --path=x and --path x both reach here as long flags after expansion,
		// so the file can be given either positionally or by name. Accepting
		// only the position would make -f, which the surface advertises, a flag
		// that parses and then does nothing.
		if v, ok := strings.CutPrefix(a, "--path="); ok {
			path = v
			continue
		}
		if a == "--path" {
			continue
		}
		if !strings.HasPrefix(a, "-") {
			path = a
		}
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, "usage: arxi blueprint validate <file.yaml>\n"+
			"short: -f path")
		os.Exit(2)
	}

	bp, err := blueprint.LoadFile(path)
	if err != nil {
		// Exit 1, not 2: the command was invoked correctly, the file is what is
		// wrong. A CI job needs to tell "you called this wrong" apart from
		// "the blueprint is invalid".
		fmt.Fprintf(os.Stderr, "blueprint is not valid.\n\n%v\n", err)
		os.Exit(1)
	}

	c := bp.Config
	name := bp.Name
	if name == "" {
		name = path
	}
	fmt.Printf("blueprint %s is valid (%d stages, %d members)\n",
		name, len(c.Stages), len(c.Members))

	fmt.Printf("  workspace: %-9s (%s)\n", c.Workspace, workspaceReason(c))

	// Stage lines are column-aligned because they are meant to be compared to
	// each other: an on_timeout that differs from its neighbours is the kind of
	// thing the eye catches in a column and misses in prose.
	wAdvance := 0
	for _, st := range c.Stages {
		if n := len("advance_when=" + st.AdvanceWhen); n > wAdvance {
			wAdvance = n
		}
	}
	for _, st := range c.Stages {
		adv := "advance_when=" + st.AdvanceWhen
		fmt.Printf("  stage %s: %-*s on_timeout=%s", st.Name, wAdvance, adv, st.OnTimeout)
		if st.TimeoutMs > 0 {
			fmt.Printf(" timeout=%s", humanMs(st.TimeoutMs))
		}
		fmt.Println()
	}

	for _, m := range c.Members {
		if m.Advisory {
			fmt.Printf("  %s is advisory: gives an opinion, does not count toward advance rules\n", m.Name)
		}
	}

	// Watchers are the only declaration that can spend money on its own, so
	// they are always shown even when the blueprint is valid.
	for _, w := range c.Watchers {
		action := w.Action
		if action == "" {
			action = "wake"
		}
		fmt.Printf("  watcher %s on %s: %s\n", w.Agent, w.Pattern, action)
	}

	fmt.Printf("  sha: %s\n", bp.SHA[:12])
}

// workspaceReason explains a resolved workspace in the user's own terms.
//
// `worktree` fires from a mechanical trigger: any member holding write, bash or
// edit. Naming those members is what turns the default from folklore into
// something the user can check, and it is the difference between accepting the
// isolation and overriding it because it looked arbitrary.
func workspaceReason(c kernel.Config) string {
	writers := writeCapableMembers(c)

	switch {
	case len(writers) == 0:
		return "resolved: no member can write"
	case len(writers) == 1:
		return "resolved: " + writers[0] + " can write"
	default:
		return "resolved: " + strings.Join(writers[:len(writers)-1], ", ") +
			" and " + writers[len(writers)-1] + " can write"
	}
}

// writeCapableMembers names the members that can change something outside the
// conversation, sorted.
//
// The write/bash/edit trio is the same test ResolveDefaults uses to pick
// `worktree` (internal/kernel/config.go: the loop that sets out.Workspace), and
// two callers need it for different sentences: workspaceReason explains why the
// isolation was chosen, and `blueprint install` warns that these are the members
// a file from somewhere else may run. Extracted rather than copied so the two
// sentences cannot come to disagree about which grants count as writing -- a
// fourth write-capable tool would otherwise be added to one of them.
func writeCapableMembers(c kernel.Config) []string {
	var writers []string
	for _, m := range c.Members {
		for _, t := range m.Tools {
			if t == "write" || t == "bash" || t == "edit" {
				writers = append(writers, m.Name)
				break
			}
		}
	}
	sort.Strings(writers)
	return writers
}

// humanMs renders a duration the way the user wrote it in their head.
// `1800000` is unreadable; the point of echoing a timeout back is to let
// somebody notice they typed one zero too many.
func humanMs(ms int64) string {
	switch {
	case ms%3600000 == 0:
		return fmt.Sprintf("%dh", ms/3600000)
	case ms%60000 == 0:
		return fmt.Sprintf("%dm", ms/60000)
	case ms%1000 == 0:
		return fmt.Sprintf("%ds", ms/1000)
	default:
		return fmt.Sprintf("%dms", ms)
	}
}
