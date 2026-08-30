package main

import (
	"fmt"
	"strings"

	"github.com/michiTrader/arxi/internal/surface"
)

// expandShort rewrites short flags into their long form before any command's
// parser sees them, using the surface's own assignment.
//
// It exists so the three parsers in this package cannot disagree about what a
// letter means. Teaching each one about `-p` separately is how `-b` ends up
// being budget in run start and base-url in provider add: nothing forces the
// two to match, and the day they differ neither is wrong on its own terms.
// Expanding first means every parser keeps handling exactly one spelling, the
// long one, and the short forms cannot drift from the registry because they are
// read out of it.
//
// The rewrite is deliberately dumb: it maps a letter to a name and stops. It
// does not validate values, so a command's own parser still rejects everything
// it would have rejected before. A pre-pass that also validated would be a
// second place where "is this flag legal here" is decided, and the two answers
// would eventually differ.
func expandShort(c *surface.Cmd, args []string) ([]string, error) {
	if c == nil {
		return args, nil
	}
	out := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		a := args[i]

		// Everything after `--` is data, not flags. Without this, a prompt that
		// begins with a dash is unpassable: `run start bp -- -p is the topic`
		// has to survive, or the escape hatch every other CLI has is missing
		// here and the user has no way to say "this really is text".
		if a == "--" {
			out = append(out, args[i:]...)
			return out, nil
		}

		// Long flags and positionals pass through untouched. `-` alone is a
		// conventional name for stdin and is not a flag.
		if !strings.HasPrefix(a, "-") || strings.HasPrefix(a, "--") || a == "-" {
			out = append(out, a)
			continue
		}

		// -b=5 is accepted, because --budget=5 is. Refusing the inline form
		// only on the short flag would make the two spellings differ in a way
		// nobody can predict from the help text.
		body, val, inline := strings.Cut(a[1:], "=")

		// Grouped booleans (-SJ) are expanded only when EVERY letter is a
		// boolean of this command. Allowing a value-taking flag inside a group
		// means `-Sb 5` has to guess whether 5 belongs to b, and a parser that
		// guesses about a spend ceiling is exactly the failure --budget's
		// mandatory-ness was designed around.
		if !inline && len(body) > 1 {
			expanded, err := expandGroup(c, body)
			if err != nil {
				return nil, err
			}
			out = append(out, expanded...)
			continue
		}

		long := c.LongFor(body)
		if long == "" {
			return nil, unknownShort(c, body)
		}
		if inline {
			out = append(out, "--"+long+"="+val)
			continue
		}
		out = append(out, "--"+long)

		// The word after a value-taking flag is its VALUE, and it is copied here
		// rather than left to the next iteration.
		//
		// This was a real bug, found by TestTheValueAfterAShortFlagIsNotTouched
		// and not by reading the code. Without it, `run prompt r1 -t "-r"`
		// expanded to `--text --run`: the user's message, which happened to
		// start with a dash, was rewritten into a different parameter. The
		// message was not rejected and not garbled in any visible way — it
		// simply became a run id, and the text the user sent was gone.
		//
		// A value is never a flag no matter what it looks like, so it is passed
		// through without inspection.
		if !isBool(c, long) && i+1 < len(args) {
			i++
			out = append(out, args[i])
		}
	}

	return out, nil
}

// expandGroup expands -SJ into --sim --json, and refuses to expand anything
// where a letter takes a value.
func expandGroup(c *surface.Cmd, body string) ([]string, error) {
	letters := strings.Split(body, "")
	out := make([]string, 0, len(letters))

	for _, l := range letters {
		long := c.LongFor(l)
		if long == "" {
			return nil, fmt.Errorf("-%s: %s has no short flag -%s. Grouped "+
				"flags like -%s are expanded letter by letter, so every letter "+
				"has to be a flag of this command.\nSee the short forms: arxi surface --flags",
				body, c.CLI(), l, body)
		}
		if !isBool(c, long) {
			return nil, fmt.Errorf("-%s cannot be grouped: -%s is --%s, which "+
				"takes a value, and inside a group there is no way to tell which "+
				"letter the value belongs to. Write it on its own: -%s VALUE",
				body, l, long, l)
		}
		out = append(out, "--"+long)
	}
	return out, nil
}

// isBool reports whether a parameter is a flag that takes no value.
func isBool(c *surface.Cmd, name string) bool {
	wire := strings.ReplaceAll(name, "-", "_")
	for _, pp := range c.WireParams() {
		if pp.Name == wire {
			return pp.Type == "bool"
		}
	}
	return false
}

// unknownShort explains a letter this command does not have.
//
// The interesting case is a letter that IS a short flag, just not here: -r is
// `run` on thirteen commands, so a user who learned it there will try it on the
// fourteenth. "unknown flag -r" sends them looking for a typo; naming what the
// letter means elsewhere tells them the truth, which is that this command has no
// such parameter.
func unknownShort(c *surface.Cmd, letter string) error {
	var owner string
	for _, pp := range surface.ShortFlags() {
		if pp.Desc == letter {
			owner = pp.Name
			break
		}
	}

	var have []string
	for _, pp := range c.WireParams() {
		cliName := strings.ReplaceAll(pp.Name, "_", "-")
		if s := surface.Short(cliName); s != "" {
			have = append(have, "-"+s+" (--"+cliName+")")
		}
	}
	list := "this command has no short flags"
	if len(have) > 0 {
		list = "it accepts: " + strings.Join(have, ", ")
	}

	if owner != "" {
		return fmt.Errorf("-%s is --%s elsewhere in the surface, but %s has no "+
			"%s parameter, so there is nothing for it to abbreviate here.\n%s",
			letter, owner, c.CLI(), owner, list)
	}
	return fmt.Errorf("-%s is not a short flag in this surface.\n%s\n"+
		"See all of them: arxi surface --flags", letter, list)
}
