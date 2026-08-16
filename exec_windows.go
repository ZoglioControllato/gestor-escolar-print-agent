//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// hiddenCommand cria um *exec.Cmd que não exibe nenhuma janela de console,
// mesmo quando o processo pai foi compilado com -H windowsgui.
func hiddenCommand(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd
}
