//go:build !windows

package logstore

func removePendingFile(path string) error {
	return removeFile(path)
}
