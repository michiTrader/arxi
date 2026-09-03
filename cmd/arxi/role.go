package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/michiTrader/arxi/internal/rolestore"
	"github.com/michiTrader/arxi/internal/surface"
)

// roleDir is where role defaults live. A var so tests can point it somewhere
// disposable, following agentDir, policyDir and providerDir.
var roleDir = rolestore.DefaultDir

// openRoles is the writer's store: it creates roles/ if it is not there.
func openRoles() *rolestore.Store {
	s, err := rolestore.Open(roleDir)
	if err != nil {
		fatal(err)
	}
	return s
}

// readRoles is the reader's store, and it creates nothing.
//
// `agent create --role X` reads this on every invocation, including the
// overwhelmingly common one in a repository where no role has ever been defined.
// Through Open, that would leave an empty roles/ behind every time an agent is
// created with a role nobody wrote -- and in a checkout the user cannot write to
// it would fail with "create roles: permission denied", an error about a
// directory the command was never asked to make, in place of the note that the
// role is not defined. rolestore.At says what a reader needs prepared, which is
// nothing: a missing directory is no roles, and a missing file is ErrNotExist.
func readRoles() *rolestore.Store {
	s, err := rolestore.At(roleDir)
	if err != nil {
		fatal(err)
	}
	return s
}

const roleUsage = "usage: arxi role define <name> [--tools a,b] [--advisory]\n" +
	"  a role is a default for `agent create --role <name>`, copied into the agent\n" +
	"  as it is written. it is stored in " + rolestore.DefaultDir + "/<name>.json, which is also the\n" +
	"  only way to read one back: the surface declares no `role list` or `role show`.\n"

// cmdRole routes the role subcommands.
//
// `define` is the only one the registry declares, so the switch has one arm, and
// the notImplemented fall-through is not dead code: it is what answers `arxi role
// list` -- a reasonable thing to try, and something this tool deliberately does
// not have -- by naming the words that do work, and it is where a second role
// verb lands the day the surface declares one.
func cmdRole(args []string) {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, roleUsage)
		os.Exit(2)
	}
	if args[0] == "define" {
		cmdRoleDefine(args[1:])
		return
	}
	notImplemented(append([]string{"role"}, args...))
}

// cmdRoleDefine writes one reusable role.
//
// Validation runs here and again in the store, following `agent create`: a bad
// name or an unknown tool is a usage error (exit 2) about something the user
// typed, and rolestore.Create re-checks because every writer must pass through
// its rules rather than trust a caller. The tool case is what earns the early
// call -- tool.ValidateGrants names EVERY bad name at once, so `--tools
// reed,gerp` is one round trip and not two, and no roles/ directory is created
// for a command that cannot succeed.
func cmdRoleDefine(args []string) {
	c := surface.Lookup("role", "define")
	vals, err := parseInvocation(c, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi role define: %v\n\n%s", err, roleUsage)
		os.Exit(2)
	}

	r := rolestore.Record{
		Name:     vals["name"],
		Advisory: vals["advisory"] == "true",
		Tools:    splitTools(vals["tools"]),
	}
	if err := r.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "arxi role define: %v\n", err)
		os.Exit(2)
	}

	st := openRoles()
	path, err := st.Create(r)
	if err != nil {
		fmt.Fprintf(os.Stderr, "arxi role define: %v\n", err)
		if errors.Is(err, rolestore.ErrExists) {
			// Exit 2, and the definition already on disk is the news. `role define`
			// is Mutates and not Idempotent in the registry, so a second call was
			// never promised to be a no-op -- and there is no --force to suggest,
			// because parseInvocation accepts only declared params and the
			// declaration has three.
			//
			// Read it rather than replace it: the agents created from the old
			// definition copied it as they were written, and nothing on disk records
			// which definition an agent came from, so an overwrite could not be
			// reviewed against the agents that inherited the previous one.
			fmt.Fprintf(os.Stderr, "  nothing was written. read the one that is there: %s\n"+
				"  or choose another name -- `role define` never overwrites.\n", st.Path(r.Name))
			os.Exit(2)
		}
		// Exit 1: the invocation was fine and the filesystem refused.
		os.Exit(1)
	}
	printRoleDefined(r, path)
}

// printRoleDefined says what was written, where, and what it will not do.
//
// Three things are printed that the flags do not already say.
//
// The resolved policy of each granted tool, for the reason `agent create` prints
// it: `--tools read,write` looks like two equal grants, and write is mutating, so
// it resolves to `ask` and a run will stop for an approval nobody expected.
// Resolved with no overrides, which is the honest answer here -- an override
// belongs to one agent, and this role has none yet.
//
// That the defaults are copied when an agent is created and never read again.
// This is the one thing about the verb that surprises people, and it surprises
// them in both directions: somebody who reads a role as live will expect
// redefining it to reach the agents that named it, and somebody who reads it as a
// template may not see that the copy is exactly what makes redefining safe.
//
// And the file, because there is nowhere else to read a role back: the surface
// declares no `role show`.
func printRoleDefined(r rolestore.Record, path string) {
	fmt.Printf("role %s defined (%s)\n", r.Name, grantSummary(r.Tools, nil))
	fmt.Printf("  file:   %s\n", path)

	// A role with nothing in it is accepted on purpose, and saying so is what
	// separates a deliberate acceptance from a command that appears to have done
	// nothing. It also states the whole reason the file is worth writing: an
	// undefined --role is reported as undefined, so registering the name is what
	// turns `--role reviewr` from an accepted string into a visible typo.
	if len(r.Tools) == 0 && !r.Advisory {
		fmt.Print("  note:   no defaults, so nothing is applied to the agents that name it --\n" +
			"          except that they can. `agent create --role` reports a role nobody\n" +
			"          has defined, which is what makes a misspelling visible.\n")
	} else {
		fmt.Print("  note:   copied into an agent as it is created, so redefining this role\n" +
			"          later changes nothing that already named it. that is deliberate:\n" +
			"          a run is judged by the rules frozen when it started.\n")
	}

	// The advisory caveat is the same one `agent create --advisory` prints, said
	// here because this is where the trait is chosen for every agent that will name
	// the role -- and said with the part that only applies here: a bool flag has no
	// negative spelling in this surface, so an agent that must work cannot opt out
	// of an advisory role. It names a different role, or none.
	if r.Advisory {
		fmt.Printf("  caveat: advisory -- an agent created with this role answers when something\n"+
			"          wakes it and takes no turn on its own, so a run of that agent alone\n"+
			"          enters its stage, activates nobody, and goes quiescent after zero\n"+
			"          turns. there is no --no-advisory: an agent that must work names\n"+
			"          another role. `arxi agent create <name> --role %s` says it again.\n", r.Name)
	}

	fmt.Printf("  use it: arxi agent create <name> --role %s --model <id>\n", r.Name)
}
