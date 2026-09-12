//go:build windows

package main

import (
	"os"
	"os/signal"
)

func notifyResize(chan<- os.Signal) {}

func notifyTerminalExit(ch chan<- os.Signal) {
	signal.Notify(ch, os.Interrupt)
}
