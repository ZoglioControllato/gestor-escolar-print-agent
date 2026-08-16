//go:build windows

package main

import (
	"context"
	"os"

	"golang.org/x/sys/windows/svc"
)

type printManagerService struct{}

func (m *printManagerService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}

	// PPE-30: o context raiz nasce **aqui**, ligado ao stop do SCM — antes desta task, `runAgent`
	// criava o próprio `context.Background()` internamente, e um `Stop`/`Shutdown` do Windows não
	// tinha como alcançá-lo (achado (ii) da fase anterior: o `cancel()` abaixo era o elo que faltava).
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := runAgent(ctx); err != nil {
			logf("Erro no agente: %v", err)
		}
	}()

	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}

	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			changes <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			// WaitHint dá ao SCM o teto real de espera (aguardar jobs em voo + a margem de
			// awaitGracefulShutdown) — sem ele, o SCM assume o default curto e pode marcar o
			// serviço como travado antes do shutdown terminar de verdade.
			waitHintMs := uint32((gracefulShutdownTimeout + gracefulShutdownGrace).Milliseconds())
			changes <- svc.Status{State: svc.StopPending, WaitHint: waitHintMs}
			cancel()
			go func() {
				awaitGracefulShutdown(done, gracefulShutdownTimeout+gracefulShutdownGrace)
				os.Exit(0)
			}()
			return false, 0
		default:
		}
	}
	return false, 0
}

func tryRunAsWindowsService() bool {
	isInteractive, err := svc.IsAnInteractiveSession()
	if err != nil || isInteractive {
		return false
	}
	isRunningAsService = true
	if err := svc.Run("GestorEscolar", &printManagerService{}); err != nil {
		logf("serviço Windows: %v", err)
	}
	return true
}
