//go:build unix

package advisorylock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func lockFile(file *os.File, wait bool) error {
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(file.Fd()), how); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return ErrWouldBlock
		}
		return fmt.Errorf("lock advisory file: %w", err)
	}
	return nil
}

func unlockFile(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("unlock advisory file: %w", err)
	}
	return nil
}
