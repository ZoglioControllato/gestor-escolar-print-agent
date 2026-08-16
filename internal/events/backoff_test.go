package events

import (
	"testing"
	"time"
)

// maxRand devolve sempre o teto do intervalo — expõe a curva do backoff sem sorteio.
func maxRand(n int64) int64 { return n }

// PPE-06: backoff exponencial 1 s → teto 60 s. A curva é o comportamento: subir devagar demais
// martela um servidor em recuperação, subir rápido demais atrasa a volta do push.
func TestBackoffDobraAteOTetoDe60s(t *testing.T) {
	cases := []struct {
		falhas int
		want   time.Duration
	}{
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, 32 * time.Second},
		{7, 60 * time.Second}, // 64 s seria o dobro; o teto corta
		{8, 60 * time.Second},
		{99, 60 * time.Second}, // saturado, sem estourar o shift
	}
	for _, tc := range cases {
		if got := backoffDelay(tc.falhas, maxRand); got != tc.want {
			t.Errorf("backoffDelay(%d) = %v, esperado %v", tc.falhas, got, tc.want)
		}
	}
	if BaseDelay != time.Second || MaxDelay != 60*time.Second {
		t.Fatalf("a spec fixa 1 s → 60 s; código diz %v → %v", BaseDelay, MaxDelay)
	}
}

// **Full jitter**, não "teto ± 10%": o sorteio cobre `[0, teto)` inteiro. É o que dissolve a rajada
// de reconexão da frota depois de um deploy — com 5 execuções reservadas na `PrintAgentFunction`
// (design.md § Risks), um pico sincronizado é o que precisa não acontecer.
func TestBackoffUsaFullJitterEmTodoOIntervalo(t *testing.T) {
	if got := backoffDelay(3, func(int64) int64 { return 0 }); got != 0 {
		t.Fatalf("com sorteio 0 o atraso deveria ser 0, veio %v", got)
	}

	var recebido int64
	_ = backoffDelay(4, func(n int64) int64 { recebido = n; return 0 })
	if recebido != int64(8*time.Second) {
		t.Fatalf("intervalo do sorteio = %v, esperado [0, 8s)", time.Duration(recebido))
	}

	// Sem injeção, o resultado precisa continuar dentro do teto.
	for i := 0; i < 200; i++ {
		d := backoffDelay(3, nil)
		if d < 0 || d >= 4*time.Second {
			t.Fatalf("backoffDelay(3) fora de [0, 4s): %v", d)
		}
	}
}

// Falha 0 ou negativa não pode virar teto negativo (shift inválido) nem atraso gigante.
func TestBackoffComContagemInvalidaCaiParaAPrimeiraFalha(t *testing.T) {
	for _, n := range []int{0, -1, -100} {
		if got := backoffDelay(n, maxRand); got != BaseDelay {
			t.Errorf("backoffDelay(%d) = %v, esperado %v", n, got, BaseDelay)
		}
	}
}

// PPE-06: "reset após 60 s estável". Sem o reset, uma conexão que cai uma vez por hora acabaria
// esperando 60 s em cada queda — a contagem nunca desceria e o push voltaria sempre tarde.
func TestResetDaContagemAposSessaoEstavel(t *testing.T) {
	cases := []struct {
		nome         string
		atual        int
		connectedFor time.Duration
		want         int
	}{
		{"sessão estável reseta para 1", 6, StableResetAfter, 1},
		{"sessão bem longa também reseta", 8, 10 * time.Minute, 1},
		{"sessão curta escala", 2, 3 * time.Second, 3},
		{"um milissegundo antes do limiar ainda escala", 4, StableResetAfter - time.Millisecond, 5},
		{"contagem satura no teto", maxFailures, time.Second, maxFailures},
		{"primeira falha", 0, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.nome, func(t *testing.T) {
			if got := nextFailureCount(tc.atual, tc.connectedFor); got != tc.want {
				t.Fatalf("nextFailureCount(%d, %v) = %d, esperado %d", tc.atual, tc.connectedFor, got, tc.want)
			}
		})
	}
	if StableResetAfter != 60*time.Second {
		t.Fatalf("a spec fixa reset após 60 s estável; código diz %v", StableResetAfter)
	}
}
