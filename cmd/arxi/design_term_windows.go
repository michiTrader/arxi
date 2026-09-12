//go:build windows

package main

import "fmt"

func rawMode(int) (func(), error) {
	return func() {}, fmt.Errorf("interactive design terminal mode is unsupported on Windows")
}

func windowSize(int) (int, int, bool) { return 0, 0, false }
