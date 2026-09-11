// Package advisorylock provides local-machine process coordination over files.
package advisorylock

import (
	"errors"
	"fmt"
	"os"
)

var ErrWouldBlock = errors.New("advisory lock is held by another process")

type Lock struct {
	file *os.File
}

func Acquire(path string, wait bool) (*Lock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open advisory lock %s: %w", path, err)
	}
	if err := lockFile(file, wait); err != nil {
		file.Close()
		return nil, err
	}
	return &Lock{file: file}, nil
}

func Held(path string) (bool, error) {
	lock, err := Acquire(path, false)
	if errors.Is(err, ErrWouldBlock) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, lock.Release()
}

func (l *Lock) File() *os.File { return l.file }

func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	if err := unlockFile(file); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
