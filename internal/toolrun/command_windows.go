//go:build windows

package toolrun

import "fmt"

func platformCommandRunner(profileDescendants string) (platformRunner, error) {
	return nil, fmt.Errorf("toolrun: Windows command execution is refused because this build has no pre-start Job Object assignment; direct-process kill is not a descendant guarantee")
}
