//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import "syscall"

// The same two ioctls under the names the BSDs give them, macOS included.
//
// TIOCSETA and not TIOCSETAW or TIOCSETAF, for the reason the Linux file gives:
// the A form takes effect immediately, and the other two wait for output to drain.
const (
	tcGetAttr = syscall.TIOCGETA
	tcSetAttr = syscall.TIOCSETA
)
