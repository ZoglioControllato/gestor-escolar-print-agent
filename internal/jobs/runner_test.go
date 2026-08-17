package jobs

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBackend imita o servidor de impressão no que importa para a unicidade:
// `GET /pending-jobs` devolve **só** o que está `queued`, e um report `printing` aceito tira o job
// da fila (é o claim do lado do servidor). Modelar isso é o que faz o teste medir o comportamento
// real em vez de uma maquete conveniente.
type fakeBackend struct {
	mu sync.Mutex

	queue    []Job
	reports  []reportCall
	fetches  int
	prints   map[string]int
	printSeq []string

	// failReport decide se um report falha. Recebe a contagem de chamadas daquele (job, status).
	failReport func(jobID, status string, callNo int) error
	reportCnt  map[string]int

	// requeueAposClaim modela um servidor que devolve o job na fila mesmo depois do claim — o pior
	// caso que o set em voo existe para cobrir.
	requeueAposClaim bool

	// printFn roda dentro de Print; permite bloquear ou falhar.
	printFn func(j Job) error

	cooldown time.Duration

	// fetched sinaliza cada fetch. Sincronizar por canal (e não por `for ... { time.Sleep }`)
	// mantém o teste determinístico: espera de verdade pelo evento, sem janela arbitrária.
	fetched chan struct{}
}

type reportCall struct {
	jobID  string
	status string
	errMsg string
}

func newFakeBackend(jobs ...Job) *fakeBackend {
	return &fakeBackend{
		queue:     append([]Job(nil), jobs...),
		prints:    map[string]int{},
		reportCnt: map[string]int{},
		fetched:   make(chan struct{}, 256),
	}
}

func (f *fakeBackend) fetch(context.Context) ([]Job, time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches++
	out := append([]Job(nil), f.queue...)
	select {
	case f.fetched <- struct{}{}:
	default:
	}
	return out, f.cooldown, nil
}

func (f *fakeBackend) report(_ context.Context, jobID, status, errMsg string) error {
	f.mu.Lock()
	key := jobID + "#" + status
	f.reportCnt[key]++
	callNo := f.reportCnt[key]
	failFn := f.failReport
	f.mu.Unlock()

	if failFn != nil {
		if err := failFn(jobID, status, callNo); err != nil {
			return err
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, reportCall{jobID, status, errMsg})
	if status == StatusPrinting && !f.requeueAposClaim {
		// Servidor real: o job deixa de estar `queued`, logo some de `/pending-jobs`.
		kept := f.queue[:0]
		for _, j := range f.queue {
			if j.ID != jobID {
				kept = append(kept, j)
			}
		}
		f.queue = kept
	}
	return nil
}

func (f *fakeBackend) print(_ context.Context, j Job) error {
	f.mu.Lock()
	f.prints[j.ID]++
	f.printSeq = append(f.printSeq, j.ID)
	fn := f.printFn
	f.mu.Unlock()
	if fn != nil {
		return fn(j)
	}
	return nil
}

func (f *fakeBackend) printCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prints[id]
}

func (f *fakeBackend) totalPrints() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.prints {
		n += c
	}
	return n
}

func (f *fakeBackend) reportsFor(id string) []reportCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []reportCall
	for _, r := range f.reports {
		if r.jobID == id {
			out = append(out, r)
		}
	}
	return out
}

// noSleep torna o backoff instantâneo e registra o que teria sido dormido — dormir de verdade num
// teste é lento e, pior, vira sincronização implícita.
func noSleep(rec *[]time.Duration, mu *sync.Mutex) func(context.Context, time.Duration) error {
	return func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		*rec = append(*rec, d)
		mu.Unlock()
		return ctx.Err()
	}
}

func newRunner(f *fakeBackend, sleep func(context.Context, time.Duration) error) *Runner {
	return New(Deps{
		Fetch:  f.fetch,
		Report: f.report,
		Print:  f.print,
		Sleep:  sleep,
	})
}

// waitFetches espera n fetches, falhando em vez de pendurar.
func (f *fakeBackend) waitFetches(t *testing.T, n int, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for i := 0; i < n; i++ {
		select {
		case <-f.fetched:
		case <-deadline:
			t.Fatalf("só %d de %d fetches em %v", i, n, within)
		}
	}
}

func jobN(n int) Job {
	return Job{ID: fmt.Sprintf("job-%d", n), PrinterExternalID: "impressora-1", DownloadURL: "https://s3/j"}
}

// ---------------------------------------------------------------------------------------------
// Unicidade
// ---------------------------------------------------------------------------------------------

// Caminho feliz: claim antes de imprimir, `completed` depois, e o job sai do registro.
func TestJobEReivindicadoAntesDeImprimirEFechadoDepois(t *testing.T) {
	f := newFakeBackend(jobN(1))
	r := newRunner(f, nil)

	if _, err := r.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	got := f.reportsFor("job-1")
	if len(got) != 2 || got[0].status != StatusPrinting || got[1].status != StatusCompleted {
		t.Fatalf("reports = %+v, esperado printing depois completed", got)
	}
	if f.printCount("job-1") != 1 {
		t.Fatalf("impressões = %d", f.printCount("job-1"))
	}
	if r.InFlight() != 0 {
		t.Fatalf("job ficou preso no registro: InFlight = %d", r.InFlight())
	}
}

// Cenário "poll concorrente": vários drains ao mesmo tempo sobre a mesma fila.
//
// Sob `-race` este teste também prova que o registro de jobs em voo é sincronizado; a contagem de
// impressões prova que ele **decide** certo. O código anterior disparava `go processJob` por job
// por poll, sem registro nenhum: aqui ele imprimiria 8 vezes.
func TestDrainsConcorrentesImprimemOJobUmaUnicaVez(t *testing.T) {
	const drains = 8
	f := newFakeBackend(jobN(1))
	f.requeueAposClaim = true // pior caso: o servidor continua devolvendo o job

	liberar := make(chan struct{})
	imprimindo := make(chan struct{}, drains)
	f.printFn = func(Job) error {
		imprimindo <- struct{}{}
		<-liberar // segura o job em voo enquanto os outros drains rodam
		return nil
	}

	// Cada drain que encontra o job já em voo é observável pelo log do runner. Sincronizar por ele
	// (e não por uma janela de tempo) é o que torna o teste determinístico: o momento de liberar a
	// impressão é exatamente aquele em que os outros 7 drains **já tentaram** e foram recusados.
	recusados := make(chan struct{}, drains)
	r := New(Deps{
		Fetch:  f.fetch,
		Report: f.report,
		Print:  f.print,
		Logf: func(format string, _ ...any) {
			if strings.Contains(format, "já em voo") {
				recusados <- struct{}{}
			}
		},
	})

	var wg sync.WaitGroup
	comecar := make(chan struct{})
	for i := 0; i < drains; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-comecar
			if _, err := r.Drain(context.Background()); err != nil {
				t.Errorf("Drain: %v", err)
			}
		}()
	}
	close(comecar)

	// A partir daqui há um job em voo de verdade.
	select {
	case <-imprimindo:
	case <-time.After(5 * time.Second):
		t.Fatal("nenhum Print começou")
	}
	for i := 0; i < drains-1; i++ {
		select {
		case <-recusados:
		case <-time.After(5 * time.Second):
			t.Fatalf("só %d de %d drains foram recusados pelo registro de jobs em voo", i, drains-1)
		}
	}
	close(liberar)
	wg.Wait()

	if n := f.printCount("job-1"); n != 1 {
		t.Fatalf("job-1 impresso %d vezes com %d drains concorrentes — o registro de jobs em voo não segurou", n, drains)
	}
	if f.fetchCount() != drains {
		t.Fatalf("fetches = %d, esperado %d (todos os drains buscaram mesmo)", f.fetchCount(), drains)
	}
}

// Cenário "evento duplicado": o Events API pode reentregar. Dois wakes, um job.
func TestEventoDuplicadoNaoImprimeDuasVezes(t *testing.T) {
	f := newFakeBackend(jobN(1))

	r := newRunner(f, nil)
	wake := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fim := make(chan struct{})
	go func() { r.Run(ctx, wake); close(fim) }()

	// Duas entregas do mesmo evento, uma depois da outra: o 2º fetch acontece com o claim já aceito,
	// e o servidor (que só devolve `queued`) não tem mais o job para dar.
	wake <- struct{}{}
	f.waitFetches(t, 1, 5*time.Second)
	wake <- struct{}{}
	f.waitFetches(t, 1, 5*time.Second)

	cancel()
	<-fim

	if n := f.printCount("job-1"); n != 1 {
		t.Fatalf("job-1 impresso %d vezes com evento duplicado", n)
	}
	if f.fetchCount() != 2 {
		t.Fatalf("fetches = %d, esperado 2 (um por entrega)", f.fetchCount())
	}
}

// O report final não é aceito e o job **continua** aparecendo na
// fila. Antes, cada poll o imprimia de novo — indefinidamente, a cada segundo.
func TestReportFinalRecusadoNaoDeixaOJobSerReimpresso(t *testing.T) {
	f := newFakeBackend(jobN(1))
	f.requeueAposClaim = true // o servidor nunca soube: o job segue na fila
	f.failReport = func(_, status string, _ int) error {
		if status == StatusCompleted {
			return errors.New("rede caiu")
		}
		return nil
	}

	var sleeps []time.Duration
	var mu sync.Mutex
	r := newRunner(f, noSleep(&sleeps, &mu))

	for ciclo := 0; ciclo < 5; ciclo++ {
		if _, err := r.Drain(context.Background()); err != nil {
			t.Fatalf("Drain %d: %v", ciclo, err)
		}
	}

	if n := f.printCount("job-1"); n != 1 {
		t.Fatalf("job-1 impresso %d vezes em 5 ciclos — é reimpressão infinita", n)
	}
	if r.InFlight() != 1 {
		t.Fatalf("InFlight = %d, esperado 1: o job precisa ficar retido enquanto o report final não é aceito", r.InFlight())
	}
}

// O outro lado: claim que não é aceito **não imprime** e devolve o job para uma tentativa
// futura. Reter aqui perderia para sempre um job cuja única falha foi uma oscilação de rede;
// imprimir seria imprimir sem o servidor saber.
func TestClaimRecusadoNaoImprimeEDevolveOJob(t *testing.T) {
	f := newFakeBackend(jobN(1))
	var claimDeveFalhar atomic.Bool
	claimDeveFalhar.Store(true)
	f.failReport = func(_, status string, _ int) error {
		if status == StatusPrinting && claimDeveFalhar.Load() {
			return errors.New("rede caiu")
		}
		return nil
	}

	var sleeps []time.Duration
	var mu sync.Mutex
	r := newRunner(f, noSleep(&sleeps, &mu))

	if _, err := r.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if f.totalPrints() != 0 {
		t.Fatalf("imprimiu %d vez(es) sem claim aceito", f.totalPrints())
	}
	if r.InFlight() != 0 {
		t.Fatalf("InFlight = %d: job sem claim aceito precisa voltar a ser tentável", r.InFlight())
	}

	// Rede volta: o mesmo job precisa imprimir, exatamente uma vez.
	claimDeveFalhar.Store(false)
	if _, err := r.Drain(context.Background()); err != nil {
		t.Fatalf("Drain 2: %v", err)
	}
	if n := f.printCount("job-1"); n != 1 {
		t.Fatalf("job-1 impresso %d vezes depois da rede voltar", n)
	}
}

// Rajada: um fetch devolve a fila inteira, e cada job sai exatamente uma vez.
func TestRajadaDeJobsImprimeCadaUmExatamenteUmaVez(t *testing.T) {
	const n = 12
	var lista []Job
	for i := 1; i <= n; i++ {
		lista = append(lista, jobN(i))
	}
	f := newFakeBackend(lista...)
	r := newRunner(f, nil)

	if _, err := r.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if f.fetchCount() != 1 {
		t.Fatalf("fetches = %d: a rajada precisa sair de um único fetch", f.fetchCount())
	}
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("job-%d", i)
		if c := f.printCount(id); c != 1 {
			t.Fatalf("%s impresso %d vezes", id, c)
		}
		got := f.reportsFor(id)
		if len(got) != 2 || got[0].status != StatusPrinting || got[1].status != StatusCompleted {
			t.Fatalf("%s reports = %+v", id, got)
		}
	}
	if r.InFlight() != 0 {
		t.Fatalf("InFlight = %d ao final da rajada", r.InFlight())
	}
	// Ordem preservada: a fila vem ordenada por `created_at` e a impressão respeita isso.
	f.mu.Lock()
	seq := append([]string(nil), f.printSeq...)
	f.mu.Unlock()
	for i, id := range seq {
		if want := fmt.Sprintf("job-%d", i+1); id != want {
			t.Fatalf("ordem de impressão[%d] = %s, esperado %s", i, id, want)
		}
	}
}

// O claim (`status=printing`) de **todos** os jobs do lote acontece antes de
// qualquer impressão física começar. Antes desta task, as duas fases estavam acopladas por job no
// mesmo laço — o claim do job N+1 só saía depois que o job N terminava de imprimir por completo
// (`Print()` bloqueia até o papel sair da impressora), medido ao vivo como 7-12s de atraso numa
// rajada real. Os outros testes de rajada (`TestRajadaDeJobsImprimeCadaUmExatamenteUmaVez`) usam
// `Print` fake instantâneo, então nunca discriminariam entre "reivindica todos, depois imprime
// todos" e "reivindica e imprime um de cada vez" — os dois produzem os mesmos reports no fim. Aqui o
// `Print` do 1º job fica bloqueado de propósito, e a asserção acontece **enquanto** ele ainda está
// bloqueado — é o único jeito de pegar esse gap.
func TestTodosOsJobsDoLoteSaoReivindicadosAntesDeQualquerImpressaoComecar(t *testing.T) {
	const n = 3
	var lista []Job
	for i := 1; i <= n; i++ {
		lista = append(lista, jobN(i))
	}
	f := newFakeBackend(lista...)

	printBlock := make(chan struct{})
	firstPrintStarted := make(chan struct{})
	var once sync.Once
	f.printFn = func(Job) error {
		once.Do(func() { close(firstPrintStarted) })
		<-printBlock
		return nil
	}

	r := newRunner(f, nil)
	drainDone := make(chan error, 1)
	go func() {
		_, err := r.Drain(context.Background())
		drainDone <- err
	}()

	<-firstPrintStarted // o 1º Print() já começou e está bloqueado, nenhum terminou ainda

	// Com o código antigo (1 passada só), só job-1 teria o claim aqui; job-2 e job-3 ainda nem
	// teriam sido tocados. Com as duas passadas, os 3 já foram reivindicados antes do 1º Print() sequer rodar.
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("job-%d", i)
		got := f.reportsFor(id)
		if len(got) < 1 || got[0].status != StatusPrinting {
			t.Fatalf("%s: reports = %+v — esperado claim (printing) antes da 1ª impressão terminar", id, got)
		}
	}

	close(printBlock) // libera job-1 e, em seguida (mesmo canal, já fechado), job-2 e job-3

	select {
	case err := <-drainDone:
		if err != nil {
			t.Fatalf("Drain: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Drain não retornou depois de liberar as impressões")
	}

	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("job-%d", i)
		if c := f.printCount(id); c != 1 {
			t.Fatalf("%s impresso %d vezes", id, c)
		}
	}
	if r.InFlight() != 0 {
		t.Fatalf("InFlight = %d ao final, esperado 0", r.InFlight())
	}
}

// O consumidor é **único e serializado**: nunca há duas impressões em curso ao mesmo tempo.
func TestRunNuncaImprimeDoisJobsAoMesmoTempo(t *testing.T) {
	var lista []Job
	for i := 1; i <= 10; i++ {
		lista = append(lista, jobN(i))
	}
	f := newFakeBackend(lista...)
	f.requeueAposClaim = true

	var emCurso atomic.Int32
	var pico atomic.Int32
	f.printFn = func(Job) error {
		n := emCurso.Add(1)
		for {
			p := pico.Load()
			if n <= p || pico.CompareAndSwap(p, n) {
				break
			}
		}
		emCurso.Add(-1)
		return nil
	}

	r := newRunner(f, nil)
	wake := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	fim := make(chan struct{})
	go func() { r.Run(ctx, wake); close(fim) }()

	wake <- struct{}{}
	f.waitFetches(t, 1, 5*time.Second)
	wake <- struct{}{}
	f.waitFetches(t, 1, 5*time.Second)
	cancel()
	<-fim

	if p := pico.Load(); p > 1 {
		t.Fatalf("pico de impressões simultâneas = %d: o consumidor não está serializado", p)
	}
}

// ---------------------------------------------------------------------------------------------
// Retry do report
// ---------------------------------------------------------------------------------------------

// 5 tentativas, backoff exponencial com teto de 2 min.
func TestReportInsisteCincoVezesComBackoffAteODoisMinutos(t *testing.T) {
	f := newFakeBackend(jobN(1))
	f.failReport = func(_, status string, _ int) error {
		if status == StatusPrinting {
			return errors.New("indisponível")
		}
		return nil
	}

	var sleeps []time.Duration
	var mu sync.Mutex
	r := newRunner(f, noSleep(&sleeps, &mu))

	if _, err := r.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	f.mu.Lock()
	tentativas := f.reportCnt["job-1#"+StatusPrinting]
	f.mu.Unlock()
	if tentativas != ReportAttempts {
		t.Fatalf("tentativas de report = %d, esperado %d", tentativas, ReportAttempts)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second, 120 * time.Second}
	if len(sleeps) != len(want) {
		t.Fatalf("esperas = %v, esperado %v", sleeps, want)
	}
	for i := range want {
		if sleeps[i] != want[i] {
			t.Fatalf("espera[%d] = %v, esperado %v", i, sleeps[i], want[i])
		}
	}
}

// A curva do backoff do report, isolada. O teto de 2 min tem que ser **alcançável**: com base de
// 1 s a escalada terminaria em 8 s e o teto existiria só no texto.
func TestReportDelayEscalaAteOTeto(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 0},
		{1, 15 * time.Second},
		{2, 30 * time.Second},
		{3, 60 * time.Second},
		{4, ReportMaxDelay},
		{5, ReportMaxDelay},
		{40, ReportMaxDelay}, // saturado, sem estourar o shift
	}
	for _, tc := range cases {
		if got := reportDelay(tc.attempt); got != tc.want {
			t.Errorf("reportDelay(%d) = %v, esperado %v", tc.attempt, got, tc.want)
		}
	}
	if ReportAttempts != 5 || ReportMaxDelay != 2*time.Minute {
		t.Fatalf("a spec fixa 5 tentativas e teto de 2 min; código diz %d/%v", ReportAttempts, ReportMaxDelay)
	}
	if reportDelay(ReportAttempts-1) != ReportMaxDelay {
		t.Fatal("o teto de 2 min não é alcançável dentro das 5 tentativas — estaria no código sem estar no comportamento")
	}
}

// Report que só falha nas primeiras tentativas acaba sendo aceito, e o job fecha normalmente.
func TestReportAceitoNaRetentativaFechaOJob(t *testing.T) {
	f := newFakeBackend(jobN(1))
	f.failReport = func(_, status string, callNo int) error {
		if status == StatusCompleted && callNo < 3 {
			return errors.New("instável")
		}
		return nil
	}
	var sleeps []time.Duration
	var mu sync.Mutex
	r := newRunner(f, noSleep(&sleeps, &mu))

	if _, err := r.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if r.InFlight() != 0 {
		t.Fatalf("InFlight = %d: report aceito na 3ª tentativa precisa liberar o job", r.InFlight())
	}
	if f.printCount("job-1") != 1 {
		t.Fatalf("impressões = %d", f.printCount("job-1"))
	}
}

// ---------------------------------------------------------------------------------------------
// Falha de impressão, rate limit e cancelamento
// ---------------------------------------------------------------------------------------------

// Impressão que falha vira `failed` **com mensagem** — o servidor precisa saber por quê.
func TestFalhaDeImpressaoReportaFailedComMensagem(t *testing.T) {
	f := newFakeBackend(jobN(1))
	f.printFn = func(Job) error { return errors.New("sem papel") }
	r := newRunner(f, nil)

	if _, err := r.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	got := f.reportsFor("job-1")
	if len(got) != 2 || got[1].status != StatusFailed {
		t.Fatalf("reports = %+v, esperado failed", got)
	}
	if got[1].errMsg != "sem papel" {
		t.Fatalf("mensagem = %q", got[1].errMsg)
	}
	if r.InFlight() != 0 {
		t.Fatalf("InFlight = %d: job com `failed` aceito está fechado", r.InFlight())
	}
}

// Erro de impressão sem mensagem não vira `errorMessage` vazio no servidor — o operador ficaria sem
// nenhuma pista, que é pior que uma pista genérica.
func TestFalhaSemMensagemGeraTextoExplicito(t *testing.T) {
	f := newFakeBackend(jobN(1))
	f.printFn = func(Job) error { return errors.New("") }
	r := newRunner(f, nil)
	if _, err := r.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	got := f.reportsFor("job-1")
	if got[1].errMsg == "" {
		t.Fatal("errorMessage vazio num report failed")
	}
}

// O cooldown do 429 é honrado **dentro** do consumidor — é isso que faz o edge case do
// "exatamente 1 fetch trailing" acontecer sem nenhuma fila de fetches.
func TestCooldownDoRateLimitEEsperadoNoConsumidor(t *testing.T) {
	f := newFakeBackend()
	f.cooldown = 10 * time.Second

	var sleeps []time.Duration
	var mu sync.Mutex
	r := newRunner(f, noSleep(&sleeps, &mu))

	cooldown, err := r.Drain(context.Background())
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if cooldown != 10*time.Second {
		t.Fatalf("Drain devolveu cooldown = %v", cooldown)
	}

	wake := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	fim := make(chan struct{})
	go func() { r.Run(ctx, wake); close(fim) }()
	wake <- struct{}{}
	// Drain não dorme: a primeira (e única) espera registrada é a do consumidor honrando o 429.
	dormiu := make(chan struct{})
	go func() {
		for {
			mu.Lock()
			n := len(sleeps)
			mu.Unlock()
			if n > 0 {
				close(dormiu)
				return
			}
			runtime.Gosched()
		}
	}()
	select {
	case <-dormiu:
	case <-time.After(5 * time.Second):
		t.Fatal("o consumidor não honrou o cooldown")
	}
	cancel()
	<-fim

	mu.Lock()
	defer mu.Unlock()
	if sleeps[0] != 10*time.Second {
		t.Fatalf("o consumidor dormiu %v, esperado o cooldown de 10s", sleeps[0])
	}
}

// Fetch com erro não derruba o consumidor: a próxima batida tenta de novo.
func TestFetchComErroNaoDerrubaOConsumidor(t *testing.T) {
	f := newFakeBackend()
	falhou := make(chan struct{}, 1)
	r := New(Deps{
		Fetch: func(ctx context.Context) ([]Job, time.Duration, error) {
			select {
			case falhou <- struct{}{}:
			default:
			}
			return nil, 0, errors.New("sem rede")
		},
		Report: f.report,
		Print:  f.print,
	})

	if _, err := r.Drain(context.Background()); err == nil {
		t.Fatal("Drain deveria propagar o erro do fetch")
	}

	wake := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	fim := make(chan struct{})
	go func() { r.Run(ctx, wake); close(fim) }()
	wake <- struct{}{}
	select {
	case <-falhou:
	case <-time.After(3 * time.Second):
		t.Fatal("o consumidor não tentou buscar depois do erro")
	}
	cancel()
	<-fim
}

// Job sem id é ignorado em vez de virar um report contra `jobId` vazio.
func TestJobSemIdEIgnorado(t *testing.T) {
	f := newFakeBackend(Job{ID: "", PrinterExternalID: "p", DownloadURL: "u"}, jobN(1))
	r := newRunner(f, nil)
	if _, err := r.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if f.totalPrints() != 1 {
		t.Fatalf("impressões = %d, esperado 1", f.totalPrints())
	}
	if len(f.reportsFor("")) != 0 {
		t.Fatal("houve report para jobId vazio")
	}
}

// Cancelamento do context raiz para o consumidor — base do graceful shutdown.
func TestCancelamentoParaOConsumidor(t *testing.T) {
	f := newFakeBackend()
	r := newRunner(f, nil)
	ctx, cancel := context.WithCancel(context.Background())
	fim := make(chan struct{})
	go func() { r.Run(ctx, make(chan struct{})); close(fim) }()
	cancel()
	select {
	case <-fim:
	case <-time.After(3 * time.Second):
		t.Fatal("Run não retornou depois do cancelamento")
	}
}

// ---------------------------------------------------------------------------------------------
// Graceful shutdown: o report final não pode se perder quando o context raiz é cancelado
// enquanto o job ainda está em voo.
// ---------------------------------------------------------------------------------------------

// Reprodução exata do defeito: o cancelamento chega **enquanto o job ainda está imprimindo** — é
// assim que o shutdown de verdade acontece, porque process() roda tudo síncrono numa única goroutine
// e o sinal (main.go) é assíncrono a isso. Antes da correção, reportWithRetry fazia `ctx.Err()` antes
// de qualquer chamada a Report, então o report final **nunca saía** — este teste falha nesse cenário
// porque só o report de claim (`printing`) apareceria em f.reportsFor("job-1").
func TestReportFinalSaiMesmoComContextoCanceladoDuranteAImpressao(t *testing.T) {
	f := newFakeBackend(jobN(1))

	printBlock := make(chan struct{})
	printStarted := make(chan struct{})
	f.printFn = func(Job) error {
		close(printStarted)
		<-printBlock
		return nil
	}

	r := newRunner(f, nil)
	ctx, cancel := context.WithCancel(context.Background())

	drainDone := make(chan error, 1)
	go func() {
		_, err := r.Drain(ctx)
		drainDone <- err
	}()

	<-printStarted
	cancel() // shutdown: o sinal chega com o documento ainda sendo impresso
	close(printBlock)

	select {
	case <-drainDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Drain não retornou depois do cancelamento com a impressão liberada")
	}

	got := f.reportsFor("job-1")
	if len(got) != 2 {
		t.Fatalf("reports = %+v, esperado 2 (printing + completed) mesmo com ctx cancelado durante a impressão — o report final não pode se perder no shutdown", got)
	}
	final := got[len(got)-1]
	if final.status != StatusCompleted {
		t.Fatalf("status do report final = %q, esperado %q", final.status, StatusCompleted)
	}
	if final.jobID != "job-1" {
		t.Fatalf("jobID do report final = %q, esperado job-1", final.jobID)
	}
	if r.InFlight() != 0 {
		t.Fatalf("InFlight = %d, esperado 0: o report final aceito precisa liberar o job", r.InFlight())
	}
}

// A mesma reprodução, mas para o outro status final (`failed`): reportFinalWithRetry não deve se
// importar com o conteúdo do status, só com o fato de ter sido cancelado.
func TestReportFinalDeFalhaSaiMesmoComContextoCanceladoDuranteAImpressao(t *testing.T) {
	f := newFakeBackend(jobN(1))

	printBlock := make(chan struct{})
	printStarted := make(chan struct{})
	f.printFn = func(Job) error {
		close(printStarted)
		<-printBlock
		return errors.New("sem papel")
	}

	r := newRunner(f, nil)
	ctx, cancel := context.WithCancel(context.Background())

	drainDone := make(chan error, 1)
	go func() {
		_, err := r.Drain(ctx)
		drainDone <- err
	}()

	<-printStarted
	cancel()
	close(printBlock)

	select {
	case <-drainDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Drain não retornou depois do cancelamento com a impressão liberada")
	}

	got := f.reportsFor("job-1")
	if len(got) != 2 {
		t.Fatalf("reports = %+v, esperado 2 (printing + failed) mesmo com ctx cancelado durante a impressão", got)
	}
	final := got[len(got)-1]
	if final.status != StatusFailed {
		t.Fatalf("status do report final = %q, esperado %q", final.status, StatusFailed)
	}
	if final.errMsg != "sem papel" {
		t.Fatalf("mensagem do report final = %q, esperado %q", final.errMsg, "sem papel")
	}
}

// FinalReportShutdownBudget precisa ser alcançável dentro das ReportAttempts tentativas da mesma
// curva de reportDelay que o caminho normal usa — senão o "respeitar o deadline restante" descrito no
// comentário de reportFinalWithRetry seria só texto: a 1ª tentativa (imediata) e a 2ª (após 15s)
// precisam caber no orçamento para o report ter uma chance real de sair no shutdown.
func TestFinalReportShutdownBudgetComportaPeloMenosDuasTentativas(t *testing.T) {
	if FinalReportShutdownBudget <= ReportBaseDelay {
		t.Fatalf("FinalReportShutdownBudget (%v) <= ReportBaseDelay (%v): a 2ª tentativa (após %v) nunca rodaria",
			FinalReportShutdownBudget, ReportBaseDelay, ReportBaseDelay)
	}
}

func (f *fakeBackend) fetchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetches
}
