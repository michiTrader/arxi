//go:build linux

package main

import "syscall"

// The two ioctls that read and write a terminal's settings, under the names Linux
// gives them. This file and design_term_bsd.go are the whole of what differs
// between kernels; everything else in design_term.go is shared.
//
// TCSETS and not TCSETSW or TCSETSF: it takes effect at once, where those two wait
// for pending output to drain first. Waiting is right for a modem and wrong here.
// Raw mode has to be on before the first frame goes out, and it has to be off
// before a traceback that may already be on its way to the same terminal.
const (
	tcGetAttr = syscall.TCGETS
	tcSetAttr = syscall.TCSETS
)
