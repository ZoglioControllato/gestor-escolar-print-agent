//go:build windows

package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"print-agent/internal/api"
)

// guiProbeTimeout limita a sondagem do painel local. É localhost: se não respondeu em 2 s, não há
// serviço escutando — e o `http.Get` sem timeout de antes deixava `--open-panel` pendurado quando
// algo segurava a porta sem responder.
const guiProbeTimeout = 2 * time.Second

var (
	guiOnce sync.Once
	guiErr  error
)

func ensureGUIServer(cfg *Config) {
	guiOnce.Do(func() {
		SetRuntimeConfig(cfg)
		guiErr = startFixedGUIServer(cfg)
		if guiErr != nil {
			logf("Aviso: painel local indisponível: %v", guiErr)
		} else {
			logf("Painel local: http://127.0.0.1:%d", fixedGUIPort)
		}
	})
}

func tryOpenGUIPanel() bool {
	url := fmt.Sprintf("http://127.0.0.1:%d", fixedGUIPort)
	ctx, cancel := context.WithTimeout(context.Background(), guiProbeTimeout)
	defer cancel()
	// Sonda "/", não "/api/status": toda rota /api/* exige o header de sessão do boot
	// de **outro** processo, que este processo (só verificando se algo escuta na porta) não tem.
	// "/" continua sem essa exigência (é ela quem entrega o token), então basta responder para
	// confirmar que o painel já está de pé.
	if _, err := apiClient.Do(ctx, api.Request{Method: http.MethodGet, URL: url + "/"}); err != nil {
		return false
	}
	openBrowserURL(url)
	return true
}

func startFixedGUIServer(cfg *Config) error {
	sessionToken, err := newGUISessionToken()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", fixedGUIPort))
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	registerGUIRoutes(mux, cfg, sessionToken)
	go http.Serve(ln, mux)
	return nil
}

func openGUI(cfg *Config) {
	ensureGUIServer(cfg)
	openGUIPanel()
}
