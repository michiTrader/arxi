package toolrun

import (
	"context"
	"os/exec"
)

// CommandSpec is the complete request given to the platform runner.
type CommandSpec struct {
	Executable string
	Argv       []string
	Script     string
	Root       string
	Env        []string
}

type platformRunner interface {
	Run(context.Context, CommandSpec, *cappedBuffer) (int, error)
}

func newCommand(spec CommandSpec) *exec.Cmd {
	cmd := exec.Command(spec.Executable, spec.Argv...)
	cmd.Dir = spec.Root
	cmd.Env = append([]string(nil), spec.Env...)
	cmd.Stdin = nil
	return cmd
}
