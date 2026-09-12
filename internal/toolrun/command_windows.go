//go:build windows

package toolrun

import (
	"context"
	"fmt"
)

type refusedWindowsRunner struct{}

func platformCommandRunner(profileDescendants string) (platformRunner, error) {
	return refusedWindowsRunner{}, nil
}

func (refusedWindowsRunner) Run(context.Context, CommandSpec, *cappedBuffer) (int, error) {
	return 0, fmt.Errorf("Windows command execution is refused because this build has no pre-start Job Object assignment; direct-process kill is not a descendant guarantee")
}
