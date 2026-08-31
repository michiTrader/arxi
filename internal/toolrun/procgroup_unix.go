//go:build !windows

package toolrun

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in a new process group of its own.
//
// This is what makes killGroup possible, and it has to happen before the start
// rather than after: a grandchild spawned in the first milliseconds would
// otherwise already belong to the old group and survive the kill.
//
// Setpgid also detaches the child from the parent's controlling terminal, so a
// command that installs a signal handler or reads a password cannot take over
// the terminal of the process supervising it.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killGroup signals every process in the group led by pgid.
//
// The negative PID is the whole mechanism: kill(-pgid) reaches the group, while
// kill(pid) reaches one process. That is why `bash -c "sleep 300 &"` outlives a
// cancelled context under the obvious implementation — the shell exits, the run
// is recorded as finished, and the grandchild keeps writing into a workspace
// nobody is watching any more.
//
// # Why the caller passes pgid instead of the Cmd
//
// The earlier version took a *exec.Cmd and called Getpgid(cmd.Process.Pid). It
// was tested, it looked right, and it silently did nothing, which is how it got
// written twice.
//
// The reason is a timing detail with no visible symptom: for `work & echo done`
// the shell exits within milliseconds, Wait reaps it immediately, and Wait then
// blocks on the output pipe that the surviving grandchild still holds open. By
// the time the deadline fires, the group LEADER is gone, so Getpgid returns
// ESRCH — the group still exists, with the orphan in it, and the lookup used to
// find it has nothing left to ask about. Measured: `Getpgid=-1 err=no such
// process` at 151ms, orphan alive.
//
// Setpgid makes the child its own group leader, so the pgid EQUALS the pid the
// caller already has at Start. Capturing it then needs no lookup and cannot go
// stale. After the fix the same probe reported `kill(-8219) = <nil>` and Wait
// returned at 151ms rather than 407ms, because killing the group also released
// the pipe.
//
// Errors are ignored on purpose: by the time this runs the group has usually
// exited, so ESRCH is the ordinary case, and reporting it would attach a
// scary-looking error to every successful command until readers learned to skip
// this line.
func killGroup(pgid int) {
	// Guard against 0 and 1: negating those signals the caller's OWN group, or
	// init. A tool runner that kills the process supervising it is one sign
	// character away, and the guard costs nothing.
	if pgid <= 1 {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}
