//go:build !windows

package main

import (
	"os"
	"syscall"
)

// restartSelf em Linux/Mac usa syscall.Exec para substituir o processo atual.
func restartSelf() {
	exe, err := os.Executable()
	if err != nil {
		logf("[UPDATE] Não foi possível reiniciar: %v", err)
		os.Exit(0)
		return
	}
	_ = syscall.Exec(exe, os.Args, os.Environ())
	os.Exit(0)
}
