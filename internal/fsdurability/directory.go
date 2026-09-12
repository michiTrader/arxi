// Package fsdurability centralizes durable filesystem publication semantics.
package fsdurability

import (
	"os"
	"runtime"
)

var openDirectory = os.Open

// SyncDirectory makes directory-entry changes durable when the platform supports
// directory handles through os.File.Sync.
func SyncDirectory(path string) error {
	dir, err := openDirectory(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return normalizeDirectorySyncError(runtime.GOOS, dir.Sync())
}
