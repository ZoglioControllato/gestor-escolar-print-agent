//go:build windows

package main

import (
	_ "embed"
	"fmt"
	"time"
	"unsafe"

	"fyne.io/systray"
	"golang.org/x/sys/windows"
)

//go:embed assets/icon.ico
var iconData []byte

func runTray(cfg *Config) {
	SetAgentName(cfg.Name)
	SetRuntimeConfig(cfg)
	systray.Run(onTrayReady, onTrayExit)
}

// getTrayConfig lê a config publicada. A cópia própria que o tray guardava (`trayCfg`) ficava
// **obsoleta** assim que a sync remota ou o painel publicavam uma nova — abrir o painel pelo tray
// reabria com o servidor antigo.
func getTrayConfig() *Config {
	return GetRuntimeConfig()
}

func onTrayReady() {
	systray.SetIcon(iconData)
	systray.SetTitle("Impressão Remota")
	// Tooltip inicial: trayStatusLoop (abaixo) publica o valor definitivo (com o nome do agente ou
	// "Desvinculado") no primeiro tick, logo depois de registrado — este é só o que aparece no
	// instante entre SetIcon e o primeiro applyTrayStatus.
	systray.SetTooltip("Impressão Remota")

	mLabel := systray.AddMenuItem("Impressão Remota", "")
	mLabel.Disable()
	systray.AddSeparator()
	mOpen := systray.AddMenuItem("Abrir painel...", "Abre a interface do agente no navegador")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Sair", "Encerrar o agente")

	go trayStatusLoop(mLabel)

	go func() {
		for {
			select {
			case <-mOpen.ClickedCh:
				openGUIPanel()
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

func onTrayExit() {}

// trayUnauthorizedLabel é o rótulo/tooltip que o tray exibe no estado "Desvinculado" (PPE-20): o
// mecanismo de revogação já funciona (401 → transportes param → re-pair único), mas antes desta
// correção o resultado nunca chegava ao tray — o operador via o agente silenciosamente parado, sem
// saber por quê (achado 2 do gate spec-driven-eval de 2026-08-10).
const trayUnauthorizedLabel = "Desvinculado — abra o painel para vincular novamente"

// trayStatusPollInterval é o intervalo de checagem de IsAgentUnauthorized() para o tray. Curto o
// bastante pra refletir a mudança em poucos segundos, sem bater na API do systray a cada tick do
// relógio.
const trayStatusPollInterval = 2 * time.Second

// trayStatusLoop mantém o tooltip e o rótulo do tray coerentes com IsAgentUnauthorized() — sem isso o
// estado "Desvinculado" existe só internamente (agent_runtime.go) e nunca aparece pra quem opera a
// máquina.
func trayStatusLoop(mLabel *systray.MenuItem) {
	applyTrayStatus(mLabel)
	ticker := time.NewTicker(trayStatusPollInterval)
	defer ticker.Stop()
	for range ticker.C {
		applyTrayStatus(mLabel)
	}
}

// applyTrayStatus escreve o tooltip/rótulo atuais no tray a partir do estado publicado
// (agent_runtime.go). Reescrever a cada tick (em vez de só na transição) é deliberado: é o mesmo nome
// já vinculado a `mLabel`/`systray`, então não há estado de "última transição" pra manter certo, e o
// custo de reescrever a mesma string é desprezível comparado ao risco de perder uma transição por um
// bug de comparação.
func applyTrayStatus(mLabel *systray.MenuItem) {
	name := GetAgentName()
	if IsAgentUnauthorized() {
		systray.SetTooltip(trayUnauthorizedLabel)
		mLabel.SetTitle(trayUnauthorizedLabel)
		return
	}
	systray.SetTooltip("Impressão Remota — " + name)
	mLabel.SetTitle("Impressão Remota")
}

var (
	shell32        = windows.NewLazySystemDLL("shell32.dll")
	procShellExecW = shell32.NewProc("ShellExecuteW")
)

func openBrowserURL(url string) {
	op, _ := windows.UTF16PtrFromString("open")
	file, _ := windows.UTF16PtrFromString(url)
	procShellExecW.Call(
		0,
		uintptr(unsafe.Pointer(op)),
		uintptr(unsafe.Pointer(file)),
		0, 0,
		1,
	)
}

func showMessageBox(title, message string) {
	user32 := windows.NewLazySystemDLL("user32.dll")
	messageBoxW := user32.NewProc("MessageBoxW")
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(message)
	messageBoxW.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), 0x10)
}

func openGUIPanel() {
	if !tryOpenGUIPanel() {
		cfg := getTrayConfig()
		if cfg != nil {
			openGUI(cfg)
		} else {
			showMessageBox("Impressão Remota", fmt.Sprintf("Abra o painel em http://127.0.0.1:%d", fixedGUIPort))
		}
	}
}

func openBrowserToPanel() {
	openBrowserURL(fmt.Sprintf("http://127.0.0.1:%d", fixedGUIPort))
}
