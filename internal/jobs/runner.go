// Package jobs é o consumidor de fila de impressão do agente: um único consumidor serializado que
// reivindica cada job antes de imprimir e nunca imprime o mesmo job duas vezes.
//
// O defeito que este pacote fecha (PPE-09, diagnostic.md §3 achado #3): o agente disparava
// `go processJob` para cada job de cada poll, sem registro de jobs em voo, e `reportStatus` era
// fire-and-forget. Quando o report falhava, o job continuava `queued` no servidor — e o poll de
// 1 segundo o buscava e o imprimia **de novo, a cada segundo**, indefinidamente. Uma escola com a
// rede oscilando via a mesma folha sair dezenas de vezes.
package jobs

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Estados que o agente reporta em `POST /print-agent/job-status`.
const (
	StatusPrinting  = "printing"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

const (
	// ReportAttempts é o total de tentativas de um report (1 inicial + 4 retentativas).
	ReportAttempts = 5

	// ReportBaseDelay é a espera antes da 1ª retentativa; dobra a cada falha.
	//
	// 15 s (e não 1 s) porque o teto da spec é 2 min: com base de 1 s a escalada terminaria em 8 s e
	// o teto nunca seria alcançado — ele estaria no código sem estar no comportamento. Com 15 s a
	// sequência é 15 s → 30 s → 60 s → 120 s, que cobre a queda de rede típica de escola sem
	// prender o consumidor por mais de ~3,5 min.
	ReportBaseDelay = 15 * time.Second

	// ReportMaxDelay é o teto entre retentativas (PPE-09).
	ReportMaxDelay = 2 * time.Minute

	// FinalReportShutdownBudget é o teto do report final (completed/failed) quando ele roda sob um
	// context já cancelado — o caso do graceful shutdown (PPE-30, achado 1 do gate spec-driven-eval de
	// 2026-08-10). Deliberadamente **menor** que `gracefulShutdownTimeout` (30 s, main.go — asserção
	// disso mesmo em `TestFinalReportShutdownBudgetFicaAbaixoDoTetoDeShutdown`, shutdown_test.go):
	// estourar o teto do shutdown não compra nenhuma tentativa extra, porque `waitForInFlightJobs` já
	// desistiu de esperar e o processo sai de qualquer forma — só atrasaria a saída sem aumentar a
	// chance de sucesso. 20 s cabe pelo menos duas tentativas reais (0 s e 15 s, a curva de
	// `reportDelay`) e ainda deixa ~10 s de folga sobre os 30 s do teto externo.
	FinalReportShutdownBudget = 20 * time.Second
)

// Job é um item da fila devolvido por `GET /print-agent/pending-jobs`.
type Job struct {
	ID                string
	PrinterExternalID string
	DownloadURL       string
}

// Deps é tudo que o runner precisa do mundo externo. Injetado para que a prova de unicidade rode
// sem rede e sem impressora.
type Deps struct {
	// Fetch busca a fila. `cooldown` > 0 é o servidor pedindo silêncio num 429 (PPE-06).
	Fetch func(ctx context.Context) (jobs []Job, cooldown time.Duration, err error)

	// Report informa o estado de um job. Erro inclui resposta não-2xx: "não registrou" e
	// "registrou" precisam ser distinguíveis, senão o anti-reimpressão não tem em que se apoiar.
	Report func(ctx context.Context, jobID, status, errMsg string) error

	// Print baixa e imprime. Bloqueia até a impressão terminar.
	Print func(ctx context.Context, j Job) error

	// Sleep é a espera do backoff e do cooldown, injetável para o teste não dormir de verdade.
	Sleep func(ctx context.Context, d time.Duration) error

	Logf func(string, ...any)
}

// Runner consome a fila. Use Run (consumidor único) ou Drain (um ciclo).
type Runner struct {
	deps Deps

	mu sync.Mutex
	// inFlight é o registro de jobs que este processo já assumiu. A entrada só sai quando o report
	// **final** é aceito — é literalmente isso que impede a reimpressão.
	inFlight map[string]struct{}
}

// New cria o runner. Dependências ausentes viram no-ops seguros em vez de nil deref.
func New(deps Deps) *Runner {
	if deps.Sleep == nil {
		deps.Sleep = sleepCtx
	}
	if deps.Logf == nil {
		deps.Logf = func(string, ...any) {}
	}
	return &Runner{deps: deps, inFlight: map[string]struct{}{}}
}

// InFlight conta os jobs que este processo assumiu e ainda não fechou.
func (r *Runner) InFlight() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.inFlight)
}

// tryClaim registra o job localmente. Devolve false se ele já está em voo — é a guarda que torna
// evento duplicado, poll concorrente e re-entrega do Events API todos inócuos.
func (r *Runner) tryClaim(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.inFlight[id]; exists {
		return false
	}
	r.inFlight[id] = struct{}{}
	return true
}

func (r *Runner) release(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.inFlight, id)
}

// Run é o **consumidor único**: um canal `wake`, um laço, um job por vez.
//
// A serialização é estrutural, não uma trava: com um só consumidor não existe o "dois polls
// concorrentes pegando o mesmo job" do código anterior, que disparava uma goroutine por job por
// poll. O canal `wake` tem capacidade 1 do lado de quem o cria, então uma rajada de eventos vira um
// ciclo — e o ciclo drena a fila inteira, não um job.
func (r *Runner) Run(ctx context.Context, wake <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		}
		cooldown, err := r.Drain(ctx)
		if err != nil {
			r.deps.Logf("[JOBS] drenagem falhou: %v", err)
		}
		if cooldown > 0 {
			// Espera o cooldown **dentro** do consumidor. É o que produz o "exatamente 1 fetch
			// trailing" do edge case da spec: enquanto dormimos, N eventos colapsam no único sinal
			// que cabe no canal, e ele é consumido logo depois — nunca uma fila de fetches.
			r.deps.Logf("[JOBS] rate limit: aguardando %v", cooldown.Round(time.Millisecond))
			if err := r.deps.Sleep(ctx, cooldown); err != nil {
				return
			}
		}
	}
}

// Drain faz **um** fetch e processa os jobs que ainda não estão em voo.
//
// Devolve o cooldown pedido pelo servidor (429), para o chamador honrá-lo.
//
// Duas passadas (PR-11, feature 038): primeiro reivindica (`status=printing`) **todos** os jobs do
// lote — é só um POST, não depende de imprimir — e só depois baixa e imprime cada um, serial. Antes
// as duas fases estavam acopladas por job dentro do mesmo laço, e o claim do job N+1 só acontecia
// depois que o job N tinha terminado de imprimir por completo (`Print()` bloqueia até o papel sair
// da impressora) — medido ao vivo em 2026-08-13: 7-12 s de atraso entre a criação do job e a
// reivindicação, numa rajada de impressões. A impressão continua serial (é o hardware —
// `TestRunNuncaImprimeDoisJobsAoMesmoTempo` não muda); só o claim deixa de esperar por ela.
func (r *Runner) Drain(ctx context.Context) (time.Duration, error) {
	queue, cooldown, err := r.deps.Fetch(ctx)
	if err != nil {
		return cooldown, err
	}

	claimed := make([]Job, 0, len(queue))
	for _, job := range queue {
		if ctx.Err() != nil {
			// Só interrompe a **passada de claim**: nenhum job daqui em diante foi reivindicado, então
			// nenhum fica pendurado — a fila continua `queued` no servidor, seguro de tentar de novo.
			return cooldown, ctx.Err()
		}
		if job.ID == "" {
			continue
		}
		if !r.tryClaim(job.ID) {
			r.deps.Logf("[JOBS] job %s já em voo — ignorado", job.ID)
			continue
		}
		if r.claim(ctx, job) {
			claimed = append(claimed, job)
		}
	}

	// Passada de impressão: **sem** checagem de `ctx.Err()` aqui, de propósito. Todo job desta lista
	// já foi reivindicado (o servidor acha que está `printing`) — abandoná-lo por causa do
	// cancelamento o deixaria preso nesse status para sempre (reabriria o PPE-30, agora para vários
	// jobs de uma vez em vez de só um). `Print()` não é cancelável de qualquer forma
	// (`cmd.CombinedOutput()` sem `CommandContext`, main.go) e `reportFinalWithRetry` já sabe sair
	// rápido sob context cancelado (`context.WithoutCancel` + `FinalReportShutdownBudget`) — a
	// garantia "reivindicado sempre recebe report final" só se sustenta se este laço nunca desistir no
	// meio. O update automático (PR-11, `checkAndApplyUpdate`) é quem evita chegar a este cenário:
	// não reinicia o processo enquanto `InFlight() > 0`.
	for _, job := range claimed {
		r.printAndReport(ctx, job)
	}
	return cooldown, nil
}

// claim reporta `printing` — antes de imprimir. O servidor só devolve `status='queued'` em
// `/pending-jobs`, então um claim aceito tira o job da fila — é o claim que fecha a janela entre
// dois fetches, e o set em voo que cobre a janela até ele ser aceito. Devolve false se o claim falhou
// definitivamente, já tendo soltado o job (nada foi impresso, soltar é seguro).
func (r *Runner) claim(ctx context.Context, job Job) bool {
	if err := r.reportWithRetry(ctx, job.ID, StatusPrinting, ""); err != nil {
		// Nada foi impresso. Soltar o job é seguro **porque** nada foi impresso, e é necessário:
		// mantê-lo preso perderia para sempre um job cuja única falha foi uma oscilação de rede.
		// Se o claim tiver chegado ao servidor e só a resposta se perdeu, o job já saiu da fila e
		// não volta — o resultado é um job que não imprime, que é o lado certo de errar quando a
		// alternativa é imprimir duas vezes.
		r.deps.Logf("[JOBS] claim do job %s falhou definitivamente: %v", job.ID, err)
		r.release(job.ID)
		return false
	}
	return true
}

// printAndReport imprime um job já reivindicado (claim já aconteceu, na passada 1 de Drain) e fecha
// com o report final.
func (r *Runner) printAndReport(ctx context.Context, job Job) {
	status, errMsg := StatusCompleted, ""
	if err := r.deps.Print(ctx, job); err != nil {
		status = StatusFailed
		errMsg = err.Error()
		if errMsg == "" {
			errMsg = "erro desconhecido (sem mensagem)"
		}
		r.deps.Logf("[JOBS] job %s falhou: %v", job.ID, err)
	}

	if err := r.reportFinalWithRetry(ctx, job.ID, status, errMsg); err != nil {
		// O job **fica** no set, de propósito e para sempre (até o processo reiniciar). Ele já foi
		// impresso e o servidor não sabe; se voltar na fila e nós o soltássemos aqui, o próximo
		// ciclo o imprimiria de novo — que é exatamente o achado #3. Um job preso é uma folha a
		// menos; um job solto é a mesma folha saindo indefinidamente.
		r.deps.Logf("[JOBS] report final do job %s não foi aceito (%v) — job segue retido para não reimprimir", job.ID, err)
		return
	}
	r.release(job.ID)
}

// reportWithRetry insiste no report com backoff exponencial (PPE-09).
func (r *Runner) reportWithRetry(ctx context.Context, jobID, status, errMsg string) error {
	var lastErr error
	for attempt := 0; attempt < ReportAttempts; attempt++ {
		if attempt > 0 {
			delay := reportDelay(attempt)
			if err := r.deps.Sleep(ctx, delay); err != nil {
				return fmt.Errorf("report %s do job %s interrompido: %w", status, jobID, err)
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = r.deps.Report(ctx, jobID, status, errMsg)
		if lastErr == nil {
			return nil
		}
		r.deps.Logf("[JOBS] report %s do job %s falhou (tentativa %d/%d): %v",
			status, jobID, attempt+1, ReportAttempts, lastErr)
	}
	return fmt.Errorf("report %s do job %s falhou em %d tentativas: %w", status, jobID, ReportAttempts, lastErr)
}

// reportFinalWithRetry envia o report **final** (completed/failed) de um job — o documento já foi
// impresso neste ponto, então perder este report é o pior desfecho possível: o job fica `printing` no
// Postgres para sempre (PPE-30, achado 1 do gate spec-driven-eval de 2026-08-10).
//
// Caminho normal (ctx vivo): delega para reportWithRetry sem nenhuma mudança de comportamento — 5
// tentativas, backoff até 2 min, preso ao ctx recebido, exatamente como o claim.
//
// Caminho de shutdown (ctx **já cancelado** ao entrar aqui — o caso real: o sinal chega enquanto o job
// ainda está em Print() ou esperando a próxima tentativa, sempre antes deste ponto, porque process()
// roda tudo síncrono numa única goroutine): reusar o mesmo ctx era o defeito — `reportWithRetry` faz
// `ctx.Err()` antes de cada `Report`, então devolvia erro **sem tentar a rede nenhuma vez**, e um
// `Sleep(ctx, ...)` preso ao mesmo ctx abortaria pelo mesmo motivo. A correção usa um context
// **desacoplado** do cancelamento (`context.WithoutCancel`) com um teto próprio, curto e menor que o
// teto de espera do shutdown — `FinalReportShutdownBudget`, não os ~3,5 min possíveis do caminho
// normal (`ReportAttempts` × `ReportMaxDelay`), que estourariam a janela de 30 s do shutdown
// (`gracefulShutdownTimeout`, main.go) sem comprar nenhuma tentativa a mais depois que
// `waitForInFlightJobs` já desistiu. Reusar `reportWithRetry` (e sua curva `reportDelay`) sobre esse
// context faz a política de tentativas se auto-limitar pelo próprio teto — `sleepCtx` retorna assim
// que o deadline do context vence, então o número efetivo de tentativas cai sozinho para o que cabe no
// orçamento, sem duplicar a curva de backoff numa segunda constante.
func (r *Runner) reportFinalWithRetry(ctx context.Context, jobID, status, errMsg string) error {
	if ctx.Err() == nil {
		return r.reportWithRetry(ctx, jobID, status, errMsg)
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), FinalReportShutdownBudget)
	defer cancel()
	return r.reportWithRetry(shutdownCtx, jobID, status, errMsg)
}

// reportDelay é a espera antes da n-ésima retentativa (n ≥ 1): 15 s, 30 s, 60 s, 120 s.
func reportDelay(attempt int) time.Duration {
	if attempt < 1 {
		return 0
	}
	delay := ReportBaseDelay << (attempt - 1)
	if delay > ReportMaxDelay || delay <= 0 {
		delay = ReportMaxDelay
	}
	return delay
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
