//go:build windows

package main

import (
	"os/exec"
	"strconv"
)

func configureBoundedTestProcess(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Killing only the parent would leave scheduled children using a test
		// directory after cleanup. taskkill's tree mode preserves the Unix helper's
		// descendant-cleanup guarantee; the fallback still reaps the parent when the
		// operating-system utility itself is unavailable.
		killTree := exec.Command("taskkill.exe", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
		if err := killTree.Run(); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
