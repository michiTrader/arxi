package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/michiTrader/arxi/internal/inbox"
	"github.com/michiTrader/arxi/internal/surface"
)

// runsDir is where run directories live, and it is a var so tests can point it
// somewhere disposable -- the same seam triggerDir and evalDir use.
var runsDir = "runs"

// cmdInbox routes the four inbox verbs.
func cmdInbox(args []string) {
	if len(args) == 0 {
		cmdInboxList(nil)
		return
	}
	switch args[0] {
	case "approve":
		cmdInboxAnswer("approve", args[1:])
	case "reject":
		cmdInboxAnswer("reject", args[1:])
	case "reply":
		cmdInboxAnswer("reply", args[1:])
	case "-h", "--help", "help":
		inboxUsage()
	default:
		// A bare `arxi inbox --json` has no subcommand. Anything that is not a
		// known verb and not a flag is most likely an id the user expected to
		// act on, and saying so beats listing the whole inbox as though nothing
		// had been typed.
		if !strings.HasPrefix(args[0], "-") {
			fmt.Fprintf(os.Stderr, "arxi inbox: %q is not an inbox command.\n"+
				"  to act on an item, name the verb first:\n"+
				"    arxi inbox approve %s\n"+
				"    arxi inbox reject  %s --reason \"...\"\n"+
				"    arxi inbox reply   %s \"...\"\n", args[0], args[0], args[0], args[0])
			os.Exit(2)
		}
		cmdInboxList(args)
	}
}

func inboxUsage() {
	fmt.Println(`usage: arxi inbox [--json]
       arxi inbox approve <id>
       arxi inbox reject  <id> --reason <why>
       arxi inbox reply   <id> <text>

Questions an agent asked and cannot continue without.

reject and reply are different acts, on purpose. reject refuses a REQUEST
and its reason reaches the agent as context; reply answers a QUESTION,
where there was nothing to authorize. Collapsing them would force the
agent to guess whether "no" meant not allowed or not that way.`)
}
