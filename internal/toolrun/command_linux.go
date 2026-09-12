//go:build linux

package toolrun

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

type linuxGroupRunner struct{}

func platformCommandRunner(profileDescendants string) (platformRunner, error) {
	if profileDescendants != "process-group" {
		return nil, fmt.Errorf("toolrun: Linux runner cannot provide descendant guarantee %q", profileDescendants)
	}
	return linuxGroupRunner{}, nil
}

func (linuxGroupRunner) Run(ctx context.Context, spec CommandSpec, buf *cappedBuffer) (int, error) {
	cmd := newCommand(spec)
	cmd.Stdout, cmd.Stderr = buf, buf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pgid := cmd.Process.Pid
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if pgid > 1 {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		case <-done:
		}
	}()
	err := cmd.Wait()
	close(done)
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	return 0, err
}
