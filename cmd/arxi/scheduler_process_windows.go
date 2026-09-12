//go:build windows

package main

import (
	"os"
	"os/exec"
	"os/signal"
)

func prepareScheduledProcess(*exec.Cmd) {}

func cancelScheduledProcess(cmd *exec.Cmd) {
	_ = cmd.Process.Kill()
}

func notifySchedulerExit(ch chan<- os.Signal) {
	signal.Notify(ch, os.Interrupt)
}
