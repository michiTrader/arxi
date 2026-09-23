//go:build !windows

package logstore

func readPendingFile(path string) ([]byte, error) {
	return readFile(path)
}
