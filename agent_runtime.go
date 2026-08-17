package main

import (
	"sync/atomic"
)

// agentRuntime é compartilhado entre o loop do agente, systray e painel HTTP local.
type agentRuntime struct {
	agentName     atomic.Value // string
	token         atomic.Value // string
	statusOnline  atomic.Bool
	lastPairError atomic.Value // string

	// unauthorized marca o estado "Desvinculado": o servidor recusou o token e nenhum
	// re-pair automático teve sucesso (ou não havia enrollmentKey salva para tentar). Só sai desse
	// estado por um pareamento que funcione — automático (handleUnauthorized) ou manual (painel).
	unauthorized atomic.Bool

	// unauthorizedHandling trava a reação a uma negativa de autorização num único episódio por vez.
	// REST (poll, heartbeat, sync) e WS podem sinalizar quase juntos — sem esta trava, cada sinal
	// viraria uma tentativa de re-pair, o loop quente que a AC proíbe.
	unauthorizedHandling atomic.Bool

	// cfg guarda a config **publicada**: uma cópia que ninguém mais mutará.
	//
	// O defeito que isto fecha: a config era um `*Config`
	// compartilhado e `syncDeviceConfig` escrevia direto no mapa dele (`cfg.PrinterTypes[k] = v`),
	// chamada por **dois** tickers (60 s e o redundante de 30 min) enquanto o painel local e as
	// goroutines de job liam o mesmo mapa. Escrita concorrente em mapa não é "resultado errado" em
	// Go: é `fatal error: concurrent map writes`, que mata o processo — uma frota que morre antes
	// de conseguir se auto-atualizar.
	//
	// A troca de um `sync.RWMutex` por cópia-e-troca não é estilo: o mutex só protegeria o ponteiro,
	// e o que corre é o **conteúdo** apontado, que segue alcançável por quem já leu.
	cfg atomic.Pointer[Config]
}

var runtimeState = &agentRuntime{}

func SetAgentName(name string) {
	runtimeState.agentName.Store(name)
}

func GetAgentName() string {
	v, _ := runtimeState.agentName.Load().(string)
	return v
}

func SetTrayToken(token string) {
	runtimeState.token.Store(token)
}

func GetAgentToken() string {
	v, _ := runtimeState.token.Load().(string)
	return v
}

func SetTrayOnline(online bool) {
	runtimeState.statusOnline.Store(online)
}

func IsAgentOnline() bool {
	return runtimeState.statusOnline.Load()
}

func SetLastPairError(msg string) {
	runtimeState.lastPairError.Store(msg)
}

func GetLastPairError() string {
	v, _ := runtimeState.lastPairError.Load().(string)
	return v
}

// SetAgentUnauthorized marca ou limpa o estado "Desvinculado".
func SetAgentUnauthorized(v bool) { runtimeState.unauthorized.Store(v) }

// IsAgentUnauthorized reporta se o agente está no estado "Desvinculado" — os produtores de tráfego
// (poll, heartbeat, sync) leem isto para não bater no servidor com um token que já sabem recusado.
func IsAgentUnauthorized() bool { return runtimeState.unauthorized.Load() }

// beginUnauthorizedEpisode entra no episódio de tratamento de 401 se nenhum outro estiver em
// andamento. Devolve false para quem chega depois — é assim que N sinais concorrentes (heartbeat e
// poll caindo quase juntos) viram **uma** reação, não uma por sinal.
func beginUnauthorizedEpisode() bool {
	return runtimeState.unauthorizedHandling.CompareAndSwap(false, true)
}

// endUnauthorizedEpisode libera a trava para a próxima negativa — resolvida esta (sucesso ou
// Desvinculado), uma negativa **nova** e futura ainda deve poder disparar uma reação.
func endUnauthorizedEpisode() {
	runtimeState.unauthorizedHandling.Store(false)
}

// SetRuntimeConfig publica uma **cópia** de cfg. Guardar o ponteiro recebido devolveria ao chamador
// uma referência viva para o estado compartilhado — exatamente o que a cópia existe para impedir.
func SetRuntimeConfig(cfg *Config) {
	if cfg == nil {
		return
	}
	runtimeState.cfg.Store(cfg.Clone())
}

// GetRuntimeConfig devolve a config publicada. O ponteiro é seguro para leitura concorrente e
// **nunca** deve ser mutado: quem precisa mudar algo chama UpdateRuntimeConfig.
func GetRuntimeConfig() *Config {
	return runtimeState.cfg.Load()
}

// UpdateRuntimeConfig aplica `mutate` sobre uma cópia e publica o resultado atomicamente.
//
// O laço de CAS existe porque há mais de um escritor (sync remota e painel local): um
// load-mutate-store simples perderia a atualização de quem publicasse primeiro, e "o nome que o
// usuário acabou de salvar sumiu" é um bug silencioso, não um erro visível.
//
// `mutate` precisa ser **pura sobre c** — ela pode ser reexecutada quando o CAS falha.
func UpdateRuntimeConfig(mutate func(c *Config)) *Config {
	for {
		current := runtimeState.cfg.Load()
		var next *Config
		if current == nil {
			next = defaultConfig()
		} else {
			next = current.Clone()
		}
		mutate(next)
		if runtimeState.cfg.CompareAndSwap(current, next) {
			return next
		}
	}
}
