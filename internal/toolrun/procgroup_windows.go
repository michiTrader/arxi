//go:build windows

package toolrun

import (
	"os"
	"os/exec"
)

// setProcessGroup is a no-op on Windows.
//
// Windows has no process groups in the POSIX sense. The equivalent is a Job
// Object, which cannot be set up through SysProcAttr alone and would need
// golang.org/x/sys — a dependency this project does not take, since it is
// standard library only and the justification does not yet exist for the one
// platform on which arxi is least likely to run a build.
func setProcessGroup(cmd *exec.Cmd) {}

// killGroup kills the single process identified by pid.
//
// This is a weaker guarantee than the unix build and is written down rather than
// left to be discovered: a grandchild backgrounded by the script survives a
// timeout here, and may keep writing into the workspace after the run was
// recorded as finished. The deadline is still enforced for the child itself, so
// the run does not hang.
//
// Not silently equivalent, not silently broken. The refusal to fake parity is
// the same reason nofollowSupported exists.
//
// The signature takes a pid rather than a *exec.Cmd to match the unix build,
// where the pid must be captured at Start because the group leader is reaped
// before the deadline fires.
func killGroup(pid int) {
	if pid <= 1 {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
