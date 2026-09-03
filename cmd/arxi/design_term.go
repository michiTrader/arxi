// design_term.go is the two things `arxi design` needs from a tty that the
// standard library does not offer: raw mode, and the window size.
//
// Both are ioctls, and both are done through syscall rather than through
// golang.org/x/term because arxi has no dependencies and will not take one on for
// sixty lines. The two constants that differ between kernels are in
// design_term_linux.go and design_term_bsd.go, and that is the whole of the
// portability story.
package main

import (
	"syscall"
	"unsafe"
)

// rawMode puts the terminal into the mode a full-screen program needs and returns
// the function that puts it back.
//
// Raw means three things here. The kernel stops assembling lines, so a key is
// readable when it is pressed rather than at the next return. It stops echoing,
// so what is typed does not appear over the frame. And it stops turning ctrl+c
// into a signal, so ctrl+c arrives as a key -- which lets the designer leave
// through the same path as every other exit, with the same restore and the same
// receipt, instead of being killed halfway through a screen.
//
// OPOST is deliberately left ON, which is not what raw mode usually means. It is
// the flag that turns a lone \n into a carriage return and a line feed, and no
// frame depends on it: draw writes \r\n itself, precisely so it does not have to.
// What does depend on it is everything this binary prints that is not a frame -- a
// panic traceback above all. With OPOST off a traceback comes out as a staircase
// walking off the right of the screen, unreadable at the one moment it is the only
// thing left to read.
//
// The error comes back untouched rather than being made into a message here,
// because the caller has a much better one: an ioctl that fails on stdin means
// stdin is not a terminal, and cmdDesign knows what to suggest doing instead.
func rawMode(fd int) (func(), error) {
	before, err := ioctlTermios(fd, tcGetAttr)
	if err != nil {
		return nil, err
	}
	raw := *before
	raw.Iflag &^= syscall.BRKINT | syscall.ICRNL | syscall.INPCK | syscall.ISTRIP | syscall.IXON
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.IEXTEN | syscall.ISIG
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	// One byte is enough to return from a read, and there is no timer: the reader
	// is a goroutine whose whole job is to block, so a timeout would only make it
	// wake up to discover that nothing was pressed.
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0

	if err := setTermios(fd, &raw); err != nil {
		return nil, err
	}
	return func() { setTermios(fd, before) }, nil
}

// windowSize is the terminal's size in columns and rows, and whether it could be
// asked at all.
//
// It is asked again on every SIGWINCH instead of being remembered, because it is
// the only input to Render that changes without a keystroke.
//
// ok is false rather than an error because every caller does the same thing with
// it: try the other descriptor, then fall back. A terminal that answers with a
// zero in either field counts as no answer -- that is what a pty reports before
// anything has set its size, and a frame of zero rows is an empty screen with no
// explanation on it.
// winsize is what TIOCGWINSZ fills in. It is declared here because package
// syscall declares Termios and not this, on every kernel arxi builds for -- the
// four fields have been in the same order since the ioctl was invented, and the
// last two are the pixel dimensions, which nothing here wants.
type winsize struct {
	Row, Col, Xpixel, Ypixel uint16
}

func windowSize(fd int) (w, h int, ok bool) {
	var ws winsize
	if err := ioctlPtr(fd, syscall.TIOCGWINSZ, unsafe.Pointer(&ws)); err != nil {
		return 0, 0, false
	}
	if ws.Col == 0 || ws.Row == 0 {
		return 0, 0, false
	}
	return int(ws.Col), int(ws.Row), true
}

// ioctlTermios reads a terminal's settings, and setTermios writes them.
//
// A pair rather than two calls at the one use site, because the settings have to
// be read into something that outlives the call -- restore() holds onto the
// original for as long as the designer runs, and it is the only copy of what the
// operator's shell looked like before.
func ioctlTermios(fd int, req uint) (*syscall.Termios, error) {
	var t syscall.Termios
	if err := ioctlPtr(fd, req, unsafe.Pointer(&t)); err != nil {
		return nil, err
	}
	return &t, nil
}

func setTermios(fd int, t *syscall.Termios) error {
	return ioctlPtr(fd, tcSetAttr, unsafe.Pointer(t))
}

// ioctlPtr is one ioctl whose third argument is a pointer to a struct the kernel
// fills in or reads out.
//
// The uintptr conversion is written inside the call to Syscall and never held in a
// variable first. That is the documented rule for unsafe.Pointer: a uintptr in a
// variable is an integer that nothing knows points anywhere, and a moving stack is
// free to invalidate it between the assignment and the call.
//
// The errno comes back as an error and is never logged here. ENOTTY on stdin is
// the expected answer inside a pipe and is what becomes cmdDesign's refusal;
// anything else is just as fatal and just as unactionable, and the caller says so
// once for both.
func ioctlPtr(fd int, req uint, p unsafe.Pointer) error {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(p))
	if e != 0 {
		return e
	}
	return nil
}
