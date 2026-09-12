package fsdurability

import (
	"errors"
	"os"
	"testing"
)

func TestSyncDirectoryPreservesOpenFailure(t *testing.T) {
	want := errors.New("open failure")
	original := openDirectory
	openDirectory = func(string) (*os.File, error) { return nil, want }
	t.Cleanup(func() { openDirectory = original })

	if err := SyncDirectory("unused"); !errors.Is(err, want) {
		t.Fatalf("SyncDirectory error = %v, want the open failure: treating directory open errors as unsupported would hide missing or inaccessible storage", err)
	}
}

func TestNormalizeDirectorySyncErrorPreservesNonWindowsFailure(t *testing.T) {
	want := errors.New("sync failure")
	if err := normalizeDirectorySyncError("linux", want); !errors.Is(err, want) {
		t.Fatalf("non-Windows sync error = %v, want %v: only the native Windows directory-fsync limitation may be ignored", err, want)
	}
}
