//go:build windows

package toolrun

import (
	"fmt"
	"os"
)

// nofollowSupported is false here, and that is a statement about the guarantee
// rather than about the code below.
//
// It is exported to the tests on purpose. Without it, the symlink tests would
// pass on Windows because the fallback happens to catch the cases they try, and
// a passing test would be read as "the final component is protected in the
// kernel" — which on this platform is not true. A weaker guarantee that says so
// is safe to build on; one that is silently weaker is not.
const nofollowSupported = false

// openNoFollow refuses a final-component symlink using an Lstat before the open.
//
// Windows has no O_NOFOLLOW, so this cannot be the same guarantee as the unix
// build and is not presented as one. The check and the open are two separate
// moments, which leaves a window: a path that is an ordinary file when Lstat
// looks at it and a link by the time OpenFile runs is not caught here.
//
// It is still worth doing. The failure this actually defends against is a model
// that wrote a symlink earlier in the same run and then wrote through it later —
// careless, not adversarial, and not racing anything. The narrow race remains,
// and is recorded rather than papered over so that nobody builds a stronger
// claim on top of it.
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	fi, err := os.Lstat(path)
	switch {
	case err == nil && fi.Mode()&os.ModeSymlink != 0:
		return nil, fmt.Errorf("toolrun: %s is a symlink, and opening it is refused\n"+
			"  the path resolved inside the workspace, but its last component is a "+
			"link that may point anywhere\n"+
			"  checked before the open rather than during it: this platform has no "+
			"O_NOFOLLOW, so a link created between the two is not caught", path)
	case err != nil && !os.IsNotExist(err):
		// A path that cannot be inspected must not be opened. Falling through to
		// the open on an unreadable Lstat would mean the one case where the check
		// could not run is the case that skips it.
		return nil, fmt.Errorf("toolrun: inspect %s before opening: %w", path, err)
	}
	return os.OpenFile(path, flag, perm)
}
