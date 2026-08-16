package events

import (
	"math/rand"
	"time"
)

const (
	// BaseDelay é o teto da primeira retentativa. Cresce dobrando até MaxDelay.
	BaseDelay = 1 * time.Second

	// MaxDelay é o teto absoluto entre tentativas. Uma escola com WSS bloqueado por firewall vai
	// bater neste valor para sempre — e é aceitável, porque o poll de fallback continua drenando a
	// fila (PPE-06); a reconexão é otimização, nunca condição de correção.
	MaxDelay = 60 * time.Second

	// StableResetAfter é quanto uma sessão precisa durar para que a contagem de falhas volte a zero.
	// Sem isso, uma conexão que cai uma vez por hora acabaria esperando 60 s em cada queda, porque a
	// contagem nunca desceria.
	StableResetAfter = 60 * time.Second

	// maxFailures limita o expoente. `BaseDelay << 6` já passa de MaxDelay; deixar a contagem
	// crescer sem teto estouraria o shift depois de algumas horas de firewall fechado.
	maxFailures = 8
)

// backoffDelay devolve o atraso da n-ésima falha consecutiva (n ≥ 1) com **full jitter**.
//
// Full jitter (sorteio uniforme em `[0, teto)`) e não "teto ± 10%": o que se está evitando é a
// frota inteira reconectando junto depois de um deploy — 5 execuções reservadas na
// `PrintAgentFunction` (design.md § Risks) não sobrevivem a uma rajada sincronizada. Jitter estreito
// mantém o pico; full jitter o dissolve.
//
// `rnd` é injetado para o teste poder fixar o sorteio nos extremos — a distribuição é o
// comportamento, não um detalhe.
func backoffDelay(failures int, rnd func(n int64) int64) time.Duration {
	if failures < 1 {
		failures = 1
	}
	if failures > maxFailures {
		failures = maxFailures
	}
	ceiling := BaseDelay << (failures - 1)
	if ceiling > MaxDelay || ceiling <= 0 {
		ceiling = MaxDelay
	}
	if rnd == nil {
		rnd = defaultRand
	}
	return time.Duration(rnd(int64(ceiling)))
}

// nextFailureCount decide a contagem de falhas depois de uma sessão que durou `connectedFor`.
//
// Sessão estável (≥ StableResetAfter) reseta para 1: a próxima queda recomeça do 1 s, não de onde a
// escalada tinha parado. Sessão curta escala — é sintoma de servidor/rede recusando de verdade.
func nextFailureCount(current int, connectedFor time.Duration) int {
	if connectedFor >= StableResetAfter {
		return 1
	}
	if current >= maxFailures {
		return maxFailures
	}
	return current + 1
}

func defaultRand(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return rand.Int63n(n)
}
