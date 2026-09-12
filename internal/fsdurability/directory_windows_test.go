//go:build windows

package fsdurability

import (
	"errors"
	"syscall"
	"testing"
)

func TestNormalizeDirectorySyncErrorIgnoresOnlyWindowsAccessDenied(t *testing.T) {
	if err := normalizeDirectorySyncError("windows", syscall.ERROR_ACCESS_DENIED); err != nil {
		t.Fatalf("Windows directory sync Access Denied = %v, want nil: native durable writes must survive the unsupported directory-fsync operation", err)
	}
	want := syscall.Errno(6) // ERROR_INVALID_HANDLE
	if err := normalizeDirectorySyncError("windows", want); !errors.Is(err, want) {
		t.Fatalf("Windows directory sync error = %v, want %v: suppressing errors beyond Access Denied would report failed durability as success", err, want)
	}
}

func TestSyncDirectoryAcceptsNativeWindowsDirectory(t *testing.T) {
	if err := SyncDirectory(t.TempDir()); err != nil {
		t.Fatalf("SyncDirectory on a native Windows directory failed: %v; durable store writes cannot complete until only the unsupported Access Denied result is normalized", err)
	}
}
