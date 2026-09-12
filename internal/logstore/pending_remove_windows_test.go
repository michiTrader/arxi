//go:build windows

package logstore

import (
	"errors"
	"syscall"
	"testing"
)

func TestPendingRemovalRetriesTransientWindowsReaderSharing(t *testing.T) {
	original := removeFile
	t.Cleanup(func() { removeFile = original })
	calls := 0
	removeFile = func(string) error {
		calls++
		if calls < 3 {
			return windowsSharingViolation
		}
		return nil
	}
	if err := removePendingFile("pending.commit"); err != nil {
		t.Fatalf("transient sharing violation was not retried: confirmed append could fail merely because a lock-free reader sampled the marker: %v", err)
	}
	if calls != 3 {
		t.Fatalf("pending removal attempts = %d, want 3: retry only the transient Windows sharing violation and stop after success", calls)
	}
}

func TestPendingRemovalPreservesPermanentWindowsFailures(t *testing.T) {
	original := removeFile
	t.Cleanup(func() { removeFile = original })
	removeFile = func(string) error { return syscall.ERROR_ACCESS_DENIED }
	if err := removePendingFile("pending.commit"); !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		t.Fatalf("pending removal error = %v, want access denied: durability failures must remain visible rather than being normalized as a reader race", err)
	}
}
