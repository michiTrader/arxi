//go:build !linux

package toolrun

import (
	"fmt"
	"os"
	"runtime"
)

func openWorkspaceRoot(string) (*os.File, error) {
	return nil, fmt.Errorf("toolrun: strong handle-relative workspace roots are unavailable on %s", runtime.GOOS)
}

func (w *Workspace) openRelative(path string, flags int, perm os.FileMode) (*os.File, error) {
	if resolved, err := w.Resolve(path); err != nil {
		return nil, err
	} else if err := w.validateToolPath(resolved); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("toolrun: strong handle-relative file access is unavailable on %s", runtime.GOOS)
}
