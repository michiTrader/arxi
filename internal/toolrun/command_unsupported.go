//go:build !linux && !windows

package toolrun

import (
	"fmt"
	"runtime"
)

func platformCommandRunner(profileDescendants string) (platformRunner, error) {
	return nil, fmt.Errorf("toolrun: command execution has no containment adapter on %s", runtime.GOOS)
}
