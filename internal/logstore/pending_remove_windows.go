package logstore

import (
	"errors"
	"syscall"
	"time"
)

const (
	windowsPendingRemovalAttempts = 20
	windowsSharingViolation       = syscall.Errno(32)
)

func removePendingFile(path string) error {
	var err error
	for attempt := 0; attempt < windowsPendingRemovalAttempts; attempt++ {
		err = removeFile(path)
		if err == nil || errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) || errors.Is(err, syscall.ERROR_PATH_NOT_FOUND) {
			return err
		}
		if !errors.Is(err, windowsSharingViolation) {
			return err
		}
		time.Sleep(time.Millisecond)
	}
	return err
}
