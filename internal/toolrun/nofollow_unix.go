//go:build !windows

package toolrun

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// nofollowSupported reports that the final-component check on this platform is
// enforced by the kernel rather than approximated.
//
// It exists so a test can assert which guarantee it is actually verifying,
// instead of passing on Windows for the wrong reason and being believed.
const nofollowSupported = true

// openNoFollow opens path without following a symlink in its FINAL component.
//
// This is the exact complement of Workspace.Resolve, and neither is redundant.
// Resolve walks the parent directories and proves they land inside the root, but
// it cannot resolve the last element because for a write the last element does
// not exist yet. O_NOFOLLOW covers precisely that last element, and it covers it
// in the kernel, in the same syscall that opens the file — so there is no window
// between the check and the use for anything to be swapped.
//
// The alternative, an Lstat before the open, is a check on a different moment in
// time than the open it is supposed to protect. On a machine running an agent
// that writes files on its own schedule, "a different moment in time" is not
// theoretical.
//
// syscall.O_NOFOLLOW is used rather than a literal, because the value is not the
// same everywhere: 0x20000 on linux/amd64, 0x8000 on linux/arm. A hardcoded
// constant would keep compiling and quietly stop meaning O_NOFOLLOW, which is
// the worst available outcome — the flag would still be passed and the
// protection would still be gone.
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(path, flag|syscall.O_NOFOLLOW, perm)
	if err == nil {
		return f, nil
	}

	// ELOOP is what Linux reports when O_NOFOLLOW refuses a symlink. FreeBSD and
	// NetBSD report EMLINK for the same condition. Both are checked because
	// matching only ELOOP would turn a caught escape into an unexplained I/O
	// error on those platforms, and an unexplained error is how a real refusal
	// gets dismissed as flakiness.
	if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK) {
		return nil, fmt.Errorf("toolrun: %s is a symlink, and opening it is refused\n"+
			"  the path resolved inside the workspace, but its last component is a "+
			"link that may point anywhere\n"+
			"  refused in the kernel by O_NOFOLLOW, in the same call that opens the "+
			"file, so nothing can be swapped in between", path)
	}
	return nil, err
}
