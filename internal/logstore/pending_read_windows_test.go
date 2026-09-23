//go:build windows

package logstore

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

func TestReadPendingRetriesTransientWindowsSharingViolation(t *testing.T) {
	original := readFile
	t.Cleanup(func() { readFile = original })
	calls := 0
	readFile = func(string) ([]byte, error) {
		calls++
		if calls < 3 {
			return nil, windowsSharingViolation
		}
		return []byte("42\n"), nil
	}
	body, err := readPendingFile("pending.commit")
	if err != nil {
		t.Fatalf("transient sharing violation was not retried on the reader side: a follower would report a corrupt log the instant a commit lands: %v", err)
	}
	if string(body) != "42\n" {
		t.Fatalf("readPendingFile returned %q, want the marker body once the violation cleared", body)
	}
	if calls != 3 {
		t.Fatalf("read attempts = %d, want 3: retry only until the transient violation clears, then stop", calls)
	}
}

func TestReadPendingRetriesTransientWindowsDeletePending(t *testing.T) {
	original := readFile
	t.Cleanup(func() { readFile = original })
	calls := 0
	readFile = func(string) ([]byte, error) {
		calls++
		if calls < 3 {
			// Delete-pending surfaces as ACCESS_DENIED until the file vanishes.
			return nil, &os.PathError{Op: "open", Path: "pending.commit", Err: syscall.ERROR_ACCESS_DENIED}
		}
		return nil, os.ErrNotExist
	}
	_, err := readPendingFile("pending.commit")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("readPendingFile error = %v, want ErrNotExist: a concurrent remove leaves the marker in a delete-pending ACCESS_DENIED state that must be retried until it resolves to the absent marker the caller expects", err)
	}
	if calls != 3 {
		t.Fatalf("read attempts = %d, want 3: the delete-pending state must be retried until the file is gone", calls)
	}
}

func TestReadPendingSurfacesAPermanentAccessDenied(t *testing.T) {
	original := readFile
	t.Cleanup(func() { readFile = original })
	calls := 0
	readFile = func(string) ([]byte, error) {
		calls++
		return nil, &os.PathError{Op: "open", Path: "pending.commit", Err: syscall.ERROR_ACCESS_DENIED}
	}
	_, err := readPendingFile("pending.commit")
	if !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		t.Fatalf("readPendingFile error = %v, want access denied: a permanent permission failure must still be surfaced so the read fails closed rather than pretending the marker is absent", err)
	}
	if calls != windowsPendingRetryAttempts {
		t.Fatalf("read attempts = %d, want %d: a permanent error is surfaced only after the bounded retry is exhausted", calls, windowsPendingRetryAttempts)
	}
}

func TestReadPendingSurfacesAnUnrelatedErrorImmediately(t *testing.T) {
	original := readFile
	t.Cleanup(func() { readFile = original })
	sentinel := errors.New("disk gone")
	calls := 0
	readFile = func(string) ([]byte, error) {
		calls++
		return nil, sentinel
	}
	_, err := readPendingFile("pending.commit")
	if !errors.Is(err, sentinel) {
		t.Fatalf("readPendingFile error = %v, want the underlying error: only the transient Windows races are retried, everything else is surfaced as-is", err)
	}
	if calls != 1 {
		t.Fatalf("read attempts = %d, want 1: a non-retryable error must not spin through the whole backoff", calls)
	}
}
