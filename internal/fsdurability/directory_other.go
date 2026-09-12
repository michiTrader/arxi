//go:build !windows

package fsdurability

func normalizeDirectorySyncError(_ string, err error) error {
	return err
}
