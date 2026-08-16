//go:build !windows

package main

import "os/exec"

// hiddenCommand em plataformas não-Windows é apenas exec.Command normal.
func hiddenCommand(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}
