//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

const windowsServiceName = "GestorEscolar"

// restartSelf substitui o processo atual pelo novo binário instalado.
// Modo serviço: para e reinicia via SCM (sc stop/start).
// Modo interativo/tray: lança novo processo e encerra o atual.
func restartSelf() {
	if isRunningAsService {
		restartAsService()
		return
	}

	exe, err := os.Executable()
	if err != nil {
		logf("[UPDATE] Não foi possível obter caminho do executável para reiniciar: %v", err)
		os.Exit(0)
		return
	}

	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		logf("[UPDATE] Erro ao iniciar novo processo: %v", err)
	}
	os.Exit(0)
}

// restartAsService para o serviço via SCM e o reinicia em uma goroutine separada,
// depois encerra o processo atual para que o SCM complete o stop limpo.
func restartAsService() {
	logf("[UPDATE] Modo serviço: reiniciando via sc stop/start...")
	go func() {
		// Aguarda o SCM processar o stop antes de iniciar
		exec.Command("sc", "stop", windowsServiceName).Run() //nolint:errcheck
		time.Sleep(3 * time.Second)
		if err := exec.Command("sc", "start", windowsServiceName).Run(); err != nil {
			logf("[UPDATE] Falha ao iniciar serviço via sc: %v", err)
		} else {
			logf("[UPDATE] Serviço reiniciado via SCM com sucesso")
		}
	}()
	// Dá tempo para o SCM registrar o stop antes de Exit
	time.Sleep(500 * time.Millisecond)
	os.Exit(0)
}
