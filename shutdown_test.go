package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"print-agent/internal/jobs"
)

// ---------------------------------------------------------------------------------------------
// PPE-30 — graceful shutdown: espera jobs em voo, respeita o teto, respeita o cancelamento
// ---------------------------------------------------------------------------------------------

// awaitGracefulShutdown devolve assim que `done` fecha, sem esperar o teto inteiro.
func TestAwaitGracefulShutdownTerminaATempoQuandoDoneFecha(t *testing.T) {
	done := make(chan struct{})
	close(done)

	start := time.Now()
	ok := awaitGracefulShutdown(done, time.Second)
	if !ok {
		t.Fatal("esperava true — done já estava fechado")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("demorou %v para retornar com done já fechado", elapsed)
	}
}

// Sem `done` fechar, awaitGracefulShutdown desiste no teto — nunca espera para sempre.
func TestAwaitGracefulShutdownEstouraOTetoSemDoneFechar(t *testing.T) {
	done := make(chan struct{}) // nunca fecha

	start := time.Now()
	ok := awaitGracefulShutdown(done, 30*time.Millisecond)
	if ok {
		t.Fatal("esperava false — done nunca fechou")
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("retornou antes do teto: %v", elapsed)
	}
}

// waitForInFlightJobs espera um job em voo terminar antes de retornar (sem teto apertado).
func TestWaitForInFlightJobsEsperaOJobTerminarDeVerdade(t *testing.T) {
	printBlock := make(chan struct{})
	printStarted := make(chan struct{})
	runner := jobs.New(jobs.Deps{
		Fetch: func(ctx context.Context) ([]jobs.Job, time.Duration, error) {
			return []jobs.Job{{ID: "job-1", PrinterExternalID: "p1", DownloadURL: "u1"}}, 0, nil
		},
		Report: func(ctx context.Context, jobID, status, errMsg string) error { return nil },
		Print: func(ctx context.Context, j jobs.Job) error {
			close(printStarted)
			<-printBlock
			return nil
		},
		Sleep: func(ctx context.Context, d time.Duration) error { return nil },
	})

	go func() { _, _ = runner.Drain(context.Background()) }()
	<-printStarted

	if runner.InFlight() != 1 {
		t.Fatalf("InFlight = %d, esperado 1 (job reivindicado, imprimindo)", runner.InFlight())
	}

	// Teto curto: o job não termina a tempo, waitForInFlightJobs desiste e o job continua em voo —
	// o teto não cancela nada, só para de esperar.
	start := time.Now()
	waitForInFlightJobs(runner, 40*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("retornou antes do teto: %v", elapsed)
	}
	if runner.InFlight() != 1 {
		t.Fatal("job deveria continuar em voo depois do teto vencer")
	}

	// Libera a impressão: agora, com folga, waitForInFlightJobs espera até o job sair de fato.
	close(printBlock)
	waitForInFlightJobs(runner, 2*time.Second)
	if runner.InFlight() != 0 {
		t.Fatal("job deveria ter saído do set em voo depois de terminar e ser liberado")
	}
}

// Sem nenhum job em voo, waitForInFlightJobs retorna quase imediatamente — o teto não é um piso.
func TestWaitForInFlightJobsSemJobsRetornaImediatamente(t *testing.T) {
	runner := jobs.New(jobs.Deps{
		Fetch:  func(ctx context.Context) ([]jobs.Job, time.Duration, error) { return nil, 0, nil },
		Report: func(ctx context.Context, jobID, status, errMsg string) error { return nil },
		Print:  func(ctx context.Context, j jobs.Job) error { return nil },
	})
	start := time.Now()
	waitForInFlightJobs(runner, time.Second)
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("demorou %v sem nenhum job em voo", elapsed)
	}
}

// runner nulo é um no-op seguro — nunca deveria acontecer em produção, mas não deve panicar.
func TestWaitForInFlightJobsRunnerNuloNaoPanica(t *testing.T) {
	waitForInFlightJobs(nil, time.Second)
}

// PPE-30: o orçamento do report final durante o shutdown (jobs.FinalReportShutdownBudget) precisa
// ficar abaixo do teto de espera do shutdown (gracefulShutdownTimeout) — senão o report nunca teria
// tempo de sequer começar antes de waitForInFlightJobs desistir de esperar e o processo sair.
func TestFinalReportShutdownBudgetFicaAbaixoDoTetoDeShutdown(t *testing.T) {
	if jobs.FinalReportShutdownBudget >= gracefulShutdownTimeout {
		t.Fatalf("jobs.FinalReportShutdownBudget (%v) >= gracefulShutdownTimeout (%v) — o report final nunca teria tempo de rodar antes do shutdown desistir",
			jobs.FinalReportShutdownBudget, gracefulShutdownTimeout)
	}
}

// ---------------------------------------------------------------------------------------------
// PPE-30 — os dois achados nomeados da fase anterior: pareamento cancelável, ctx do SCM
// ---------------------------------------------------------------------------------------------

// Achado (i): o laço de pareamento respeita ctx.Done() — um SIGTERM durante o pareamento não espera
// os 60 s inteiros do backoff entre tentativas.
func TestRunDuranteOPareamentoRespeitaCancelamentoDeContexto(t *testing.T) {
	// /pair sempre falha: o laço de pareamento de run() nunca sai do sleepCtx(ctx, 60s) sozinho —
	// só o cancelamento de ctx pode tirá-lo de lá, que é exatamente o que este teste prova.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := &Config{
		Server:    srv.URL,
		Name:      "Agente de Teste",
		TokenFile: filepath.Join(t.TempDir(), "token.txt"), // ausente: força o laço de pareamento
	}
	t.Cleanup(func() { _ = os.Remove(filepath.Join(configDataDir(), "config.json")) })
	resetRuntimeConfig(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, cfg) }()

	// Dá tempo de UMA tentativa de pair falhar e entrar no sleepCtx de 60s.
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case <-errCh:
		// run() retornou — é o que prova que o cancelamento não esperou os 60s do backoff.
	case <-time.After(5 * time.Second):
		t.Fatal("run() não retornou em 5s depois do cancelamento durante o pareamento — o antigo time.Sleep(60s) ressuscitou")
	}
}

// Achado (ii): runAgent recebe o context de fora — se ele nascesse por dentro
// (context.Background()), um cancelamento externo não teria efeito nenhum. A prova é de
// assinatura/comportamento: runAgent(ctx) devolve rápido quando ctx já chega cancelado, porque loadConfig
// já não depende de ctx mas run() sim.
func TestRunAgentRecebeOContextDeFora(t *testing.T) {
	// runAgent chama loadConfig() (sem override de diretório — ver a nota em secure_test.go) e
	// run() cria os arquivos de dados de qualquer forma, mesmo com ctx já cancelado (ensureDataFiles
	// não olha ctx): limpa o que fica no caminho real compartilhado.
	t.Cleanup(func() {
		_ = os.Remove(filepath.Join(configDataDir(), "config.json"))
		_ = os.Remove(filepath.Join(configDataDir(), "token.txt"))
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // já cancelado antes de chamar

	done := make(chan error, 1)
	go func() { done <- runAgent(ctx) }()

	select {
	case <-done:
		// runAgent(ctx) devolveu rápido porque o ctx (recebido de fora, não criado por dentro)
		// já estava cancelado — se ele criasse context.Background() internamente, o cancelamento
		// externo não alcançaria run() e o teste estouraria o timeout abaixo.
	case <-time.After(3 * time.Second):
		t.Fatal("runAgent(ctx) não terminou com um context já cancelado — o context não está vindo de fora")
	}
}
