//go:build windows

package fsdurability

import (
	"errors"
	"syscall"
)

func normalizeDirectorySyncError(platform string, err error) error {
	if platform == "windows" && errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		return nil
	}
	return err
}
