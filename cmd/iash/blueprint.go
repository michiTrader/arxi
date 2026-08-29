package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/michiTrader/iash/internal/blueprint"
	"github.com/michiTrader/iash/internal/kernel"
	"github.com/michiTrader/iash/internal/surface"
)

// cmdBlueprintValidate implements `iash blueprint validate <path>`.
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
		fmt.Fprintf(os.Stderr, "iash blueprint validate: %v\n", err)
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
		fmt.Fprintln(os.Stderr, "usage: iash blueprint validate <file.yaml>\n"+
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
