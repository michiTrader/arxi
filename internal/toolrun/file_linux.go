//go:build linux

package toolrun

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func (w *Workspace) openRelative(path string, flags int, perm os.FileMode) (*os.File, error) {
	parts, err := relativeParts(path)
	if err != nil {
		return nil, fmt.Errorf("toolrun: %s: %w", w.Member, err)
	}
	root, err := syscall.Open(w.Root, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("toolrun: open workspace root: %w", err)
	}
	current := root
	for _, component := range parts[:len(parts)-1] {
		next, openErr := syscall.Openat(current, component, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
		if current != root {
			_ = syscall.Close(current)
		}
		if openErr != nil {
			_ = syscall.Close(root)
			return nil, fmt.Errorf("toolrun: refuse parent component %q: %w", component, openErr)
		}
		current = next
	}
	fd, openErr := syscall.Openat(current, parts[len(parts)-1], flags|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, uint32(perm.Perm()))
	if current != root {
		_ = syscall.Close(current)
	}
	_ = syscall.Close(root)
	if openErr != nil {
		if openErr == syscall.ELOOP {
			return nil, fmt.Errorf("toolrun: refuse final symlink %q: %w", parts[len(parts)-1], openErr)
		}
		return nil, fmt.Errorf("toolrun: refuse final component %q: %w", parts[len(parts)-1], openErr)
	}
	return os.NewFile(uintptr(fd), filepath.Base(path)), nil
}

func relativeParts(path string) ([]string, error) {
	if path == "" || filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return nil, fmt.Errorf("path %q must be non-empty and relative", path)
	}
	clean := filepath.Clean(path)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("path %q escapes the workspace", path)
	}
	return strings.Split(clean, string(filepath.Separator)), nil
}

func readBounded(file *os.File, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(file, limit))
}
