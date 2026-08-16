package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"print-agent/internal/events"
)

// ---------------------------------------------------------------------------------------------
// PPE-28 / PPE-10 — o bloco `events` decide push × poll
// ---------------------------------------------------------------------------------------------

// A resposta real do servidor com a flag ligada (`print-agent-device-config.int.test.ts`).
const deviceConfigComEvents = `{
  "printerTypes": {"ext-1": "a4"},
  "printSettings": "noscale",
  "events": {
    "enabled": true,
    "realtimeEndpoint": "events-dev.pedagogicoonline.com.br",
    "httpHost": "events-dev.pedagogicoonline.com.br",
    "channel": "/print/conta-1/device-1"
  }
}`

// A resposta com a flag desligada — byte-idêntica à resposta pré-feature (prova de aditividade).
const deviceConfigSemEvents = `{"printerTypes":{"ext-1":"a4"},"printSettings":"noscale"}`

// PPE-10/PPE-28: o bloco `events` chega do device-config e vira a configuração de push.
func TestBlocoEventsDoDeviceConfigViraConfiguracaoDePush(t *testing.T) {
	p, err := decodeDevicePolicy([]byte(deviceConfigComEvents))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.events == nil {
		t.Fatal("bloco events não foi lido")
	}
	if p.events.RealtimeEndpoint != "events-dev.pedagogicoonline.com.br" {
		t.Fatalf("realtimeEndpoint = %q", p.events.RealtimeEndpoint)
	}
	if p.events.Channel != "/print/conta-1/device-1" {
		t.Fatalf("channel = %q", p.events.Channel)
	}
	// PPE-34: o host entregue ao agente é o domínio customizado, nunca `*.amazonaws.com`.
	if strings.Contains(p.events.HTTPHost, "amazonaws.com") {
		t.Fatalf("httpHost = %q: o agente nunca deve receber host regional", p.events.HTTPHost)
	}
}

// PPE-28: bloco ausente **ou** `enabled:false` significam a mesma coisa — opere em poll.
func TestBlocoAusenteOuDesligadoSignificaModoPoll(t *testing.T) {
	cases := map[string]string{
		"bloco ausente (frota 1.x / kill-switch)": deviceConfigSemEvents,
		"enabled:false": `{"events":{"enabled":false,"realtimeEndpoint":"h","httpHost":"h","channel":"/print/a/b"}}`,
		"events null":   `{"events":null}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p, err := decodeDevicePolicy([]byte(body))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if p.events != nil {
				t.Fatalf("events = %+v, esperado nulo (modo poll)", p.events)
			}
		})
	}
}

// PPE-10: o kill-switch devolve a frota ao poll em ≤60 s **sem redeploy de agente**. O caminho é a
// próxima sync: o bloco some da resposta e precisa apagar o que estava publicado.
func TestKillSwitchApagaAConfiguracaoDePushNaProximaSync(t *testing.T) {
	resetRuntimeConfig(t, testConfig())

	ligado, err := decodeDevicePolicy([]byte(deviceConfigComEvents))
	if err != nil {
		t.Fatalf("decode ligado: %v", err)
	}
	UpdateRuntimeConfig(func(c *Config) { applyDevicePolicy(c, ligado) })
	if GetRuntimeConfig().Events == nil {
		t.Fatal("push não ficou configurado com a flag ligada")
	}
	if _, ok := eventsClientConfig(GetRuntimeConfig(), "tok"); !ok {
		t.Fatal("eventsClientConfig recusou uma configuração completa")
	}

	desligado, err := decodeDevicePolicy([]byte(deviceConfigSemEvents))
	if err != nil {
		t.Fatalf("decode desligado: %v", err)
	}
	UpdateRuntimeConfig(func(c *Config) { applyDevicePolicy(c, desligado) })

	if got := GetRuntimeConfig().Events; got != nil {
		t.Fatalf("Events = %+v depois do kill-switch: um merge deixaria o endpoint antigo vivo para sempre", got)
	}
	if _, ok := eventsClientConfig(GetRuntimeConfig(), "tok"); ok {
		t.Fatal("o transporte continuaria ligado depois do kill-switch")
	}
	// O resto da política continua valendo — o kill-switch desliga o push, não a sincronização.
	if GetRuntimeConfig().PrintSettings != "noscale" {
		t.Fatalf("PrintSettings = %q", GetRuntimeConfig().PrintSettings)
	}
}

// Configuração incompleta nunca vira tentativa de conexão contra host vazio.
func TestEventsClientConfigRecusaConfiguracaoIncompleta(t *testing.T) {
	completa := &EventsConfig{Enabled: true, RealtimeEndpoint: "h", HTTPHost: "h", Channel: "/print/a/b"}
	cases := []struct {
		nome  string
		cfg   *Config
		token string
	}{
		{"config nula", nil, "tok"},
		{"sem bloco events", &Config{}, "tok"},
		{"bloco desligado", &Config{Events: &EventsConfig{Enabled: false, RealtimeEndpoint: "h", HTTPHost: "h", Channel: "/c"}}, "tok"},
		{"sem token (ainda não pareado)", &Config{Events: completa}, ""},
		{"sem canal", &Config{Events: &EventsConfig{Enabled: true, RealtimeEndpoint: "h", HTTPHost: "h"}}, "tok"},
		{"sem endpoint", &Config{Events: &EventsConfig{Enabled: true, HTTPHost: "h", Channel: "/c"}}, "tok"},
	}
	for _, tc := range cases {
		t.Run(tc.nome, func(t *testing.T) {
			if _, ok := eventsClientConfig(tc.cfg, tc.token); ok {
				t.Fatal("aceitou configuração que não permite push")
			}
		})
	}
	if _, ok := eventsClientConfig(&Config{Events: completa}, "tok"); !ok {
		t.Fatal("recusou configuração completa")
	}
}

// ---------------------------------------------------------------------------------------------
// PPE-06 / PPE-26 — seletor de intervalo
// ---------------------------------------------------------------------------------------------

// O ritmo do poll depende do transporte: 10 s com o WebSocket fora (sem regressão de UX — é a
// latência efetiva de hoje) e 300 s com ele assinado (só reconciliação).
func TestIntervaloDoPollDependeDoEstadoDoTransporte(t *testing.T) {
	cases := []struct {
		status events.Status
		base   time.Duration
		jitter float64
	}{
		{events.StatusDisconnected, pollFallbackInterval, pollFallbackJitter},
		// `connecting` ainda **não** garante entrega: tratá-lo como saudável abriria uma janela de
		// até 300 s sem push e sem poll.
		{events.StatusConnecting, pollFallbackInterval, pollFallbackJitter},
		{events.StatusSubscribed, pollReconcileInterval, pollReconcileJitter},
	}
	for _, tc := range cases {
		t.Run(tc.status.String(), func(t *testing.T) {
			base, jitter := pollBaseInterval(tc.status)
			if base != tc.base || jitter != tc.jitter {
				t.Fatalf("pollBaseInterval(%v) = (%v, %v), esperado (%v, %v)", tc.status, base, jitter, tc.base, tc.jitter)
			}
		})
	}
	if pollFallbackInterval != 10*time.Second || pollReconcileInterval != 300*time.Second {
		t.Fatalf("a spec fixa 10 s / 300 s; código diz %v / %v", pollFallbackInterval, pollReconcileInterval)
	}
	// A redução de ≥90% no volume depende disto: 1 s → 10 s já é 10×; com o push assinado, 300×.
	if antes := 1 * time.Second; pollFallbackInterval/antes < 10 {
		t.Fatal("o intervalo de fallback não reduz o volume o suficiente")
	}
}

// Jitter espalha o intervalo em ±frac. Sem ele, a frota inteira bate no mesmo segundo depois de um
// deploy — e a `PrintAgentFunction` tem 5 execuções reservadas (design.md § Risks).
func TestJitterEspalhaOIntervaloNosDoisSentidos(t *testing.T) {
	base := 10 * time.Second

	if got := withJitter(base, 0.2, func() float64 { return 0 }); got != 8*time.Second {
		t.Fatalf("extremo inferior = %v, esperado 8s (-20%%)", got)
	}
	if got := withJitter(base, 0.2, func() float64 { return 1 }); got != 12*time.Second {
		t.Fatalf("extremo superior = %v, esperado 12s (+20%%)", got)
	}
	if got := withJitter(base, 0.2, func() float64 { return 0.5 }); got != base {
		t.Fatalf("centro = %v, esperado %v", got, base)
	}
	if got := withJitter(base, 0, nil); got != base {
		t.Fatalf("jitter zero deveria devolver a base, veio %v", got)
	}

	// Sem injeção, tudo dentro da faixa e nunca negativo.
	for i := 0; i < 500; i++ {
		d := nextPollDelay(events.StatusDisconnected, nil)
		if d < 8*time.Second || d > 12*time.Second {
			t.Fatalf("nextPollDelay fora de [8s,12s]: %v", d)
		}
		r := nextPollDelay(events.StatusSubscribed, nil)
		if r < 270*time.Second || r > 330*time.Second {
			t.Fatalf("nextPollDelay (reconciliação) fora de [270s,330s]: %v", r)
		}
	}
}

// ---------------------------------------------------------------------------------------------
// PPE-02 — coalescing
// ---------------------------------------------------------------------------------------------

// O canal `wake` tem capacidade 1: N sinais viram 1 fetch. É o coalescing do edge case da spec, e
// é o que impede que uma rajada de 20 jobs vire 20 chamadas a `/pending-jobs` — 19 delas batendo
// no rate limit e devolvendo a mesma fila.
func TestSinaisConcorrentesCoalescemEmUmUnicoCiclo(t *testing.T) {
	wake := make(chan struct{}, 1)

	for i := 0; i < 50; i++ {
		signalWake(wake)
	}
	if got := len(wake); got != 1 {
		t.Fatalf("sinais pendentes = %d, esperado 1", got)
	}
	<-wake
	select {
	case <-wake:
		t.Fatal("havia mais de um sinal enfileirado")
	default:
	}

	// E nunca bloqueia: um produtor preso aqui congelaria o ticker ou o cliente WebSocket.
	pronto := make(chan struct{})
	go func() {
		signalWake(wake)
		signalWake(wake)
		close(pronto)
	}()
	select {
	case <-pronto:
	case <-time.After(2 * time.Second):
		t.Fatal("signalWake bloqueou")
	}
}

// ---------------------------------------------------------------------------------------------
// PPE-07 — heartbeat
// ---------------------------------------------------------------------------------------------

// O `transport` do heartbeat é a telemetria que identifica as escolas cujo firewall bloqueia WSS.
func TestRotuloDeTransporteSeguirOEstadoReal(t *testing.T) {
	if got := transportLabel(events.StatusSubscribed); got != "ws" {
		t.Fatalf("subscribed → %q", got)
	}
	// `connecting` **não** é `ws`: reportar assim inflaria a adoção com conexões que nunca fecharam.
	for _, s := range []events.Status{events.StatusDisconnected, events.StatusConnecting} {
		if got := transportLabel(s); got != "poll" {
			t.Fatalf("%v → %q, esperado poll", s, got)
		}
	}
}

// PPE-07: o heartbeat bate na rota nova, com o corpo que o servidor lê (`print-agent.ts:319`).
//
// Sem esta batida, um agente **saudável** (poll de reconciliação a 300 s) estouraria o limite de
// 150 s do servidor e a escola receberia 409 ao mandar imprimir — o push teria trocado polling
// excessivo por impressão recusada.
func TestHeartbeatEnviaTransporteEVersaoNaRotaNova(t *testing.T) {
	type recebido struct {
		path  string
		auth  string
		corpo map[string]string
	}
	got := make(chan recebido, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]string
		_ = json.Unmarshal(body, &m)
		got <- recebido{path: r.URL.Path, auth: r.Header.Get("Authorization"), corpo: m}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"agentOnlineThresholdS":150}`))
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.Server = srv.URL
	if err := sendHeartbeat(context.Background(), cfg, "tok-123", "ws"); err != nil {
		t.Fatalf("sendHeartbeat: %v", err)
	}

	r := <-got
	if r.path != "/print-agent/heartbeat" {
		t.Fatalf("path = %q", r.path)
	}
	if r.auth != "Bearer tok-123" {
		t.Fatalf("Authorization = %q", r.auth)
	}
	if r.corpo["transport"] != "ws" {
		t.Fatalf("transport = %q", r.corpo["transport"])
	}
	if r.corpo["agentVersion"] == "" {
		t.Fatal("agentVersion ausente: é a telemetria de adoção da frota")
	}
	if heartbeatInterval != 60*time.Second {
		t.Fatalf("a spec fixa 60 s; código diz %v", heartbeatInterval)
	}
	// 2,5 batidas cabem no limite de 150 s do servidor — é o que tolera uma batida perdida.
	if heartbeatInterval*2 >= 150*time.Second {
		t.Fatal("o intervalo do heartbeat não tolera uma batida perdida dentro do limite de 150 s")
	}
}

// Heartbeat recusado é erro, não silêncio: o agente precisa poder logar que a presença não subiu.
func TestHeartbeatComRespostaDeErroDevolveErro(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	cfg := testConfig()
	cfg.Server = srv.URL
	if err := sendHeartbeat(context.Background(), cfg, "tok", "poll"); err == nil {
		t.Fatal("heartbeat 401 deveria devolver erro")
	}
}

// ---------------------------------------------------------------------------------------------
// Supervisor do transporte
// ---------------------------------------------------------------------------------------------

// A configuração de push muda em runtime (kill-switch, re-pareamento, stack publicando depois). O
// supervisor liga, desliga e reinicia sem reiniciar o agente — mas **não** reabre a conexão quando
// nada mudou: um handshake por sync de 60 s ressincronizaria a frota inteira no mesmo minuto.
func TestSupervisorNaoReabreConexaoQuandoNadaMuda(t *testing.T) {
	tr := &transport{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wake := make(chan struct{}, 1)

	cfg := events.Config{RealtimeEndpoint: "127.0.0.1:1", HTTPHost: "h", Channel: "/print/a/b", Token: "tok"}
	tr.apply(ctx, cfg, true, wake)
	tr.mu.Lock()
	primeiro := tr.client
	tr.mu.Unlock()
	if primeiro == nil {
		t.Fatal("transporte não ligou")
	}

	tr.apply(ctx, cfg, true, wake)
	tr.mu.Lock()
	segundo := tr.client
	tr.mu.Unlock()
	if segundo != primeiro {
		t.Fatal("o supervisor recriou o cliente com a mesma configuração")
	}

	// Token novo (re-pareamento) precisa reabrir: o antigo já não autoriza nada.
	novo := cfg
	novo.Token = "tok-2"
	tr.apply(ctx, novo, true, wake)
	tr.mu.Lock()
	terceiro := tr.client
	tr.mu.Unlock()
	if terceiro == primeiro {
		t.Fatal("o supervisor manteve o cliente com token trocado")
	}

	// Kill-switch: desliga e o estado volta a `disconnected`.
	tr.apply(ctx, events.Config{}, false, wake)
	tr.mu.Lock()
	depois := tr.client
	tr.mu.Unlock()
	if depois != nil {
		t.Fatal("o transporte continuou ligado depois do kill-switch")
	}
	if tr.status() != events.StatusDisconnected {
		t.Fatalf("status = %v depois de desligar", tr.status())
	}
}

// Sem transporte, o status é `disconnected` — e é isso que mantém o poll em 10 s.
func TestTransporteSemClienteReportaDesconectado(t *testing.T) {
	tr := &transport{}
	if got := tr.status(); got != events.StatusDisconnected {
		t.Fatalf("status = %v", got)
	}
	if got := transportLabel(tr.status()); got != "poll" {
		t.Fatalf("rótulo = %q", got)
	}
}

// ---------------------------------------------------------------------------------------------
// O ticker de 1 segundo
// ---------------------------------------------------------------------------------------------

// O ticker de 1 s era a causa das 86.400 requisições/dia/device. Este teste é a versão executável
// do "substituir o ticker de 1 s" da task: ele não pode voltar por descuido numa PR futura.
func TestTickerDeUmSegundoNaoExisteMais(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("ler main.go: %v", err)
	}
	// A varredura mira o **ticker**, não qualquer literal de 1 segundo: `main()` tem um
	// `time.Sleep(1 * time.Second)` legítimo no retry de `--open-panel`, que nunca teve nada a ver
	// com o poll. Uma varredura larga demais mandaria a próxima sessão enfraquecer a asserção para
	// fazê-la passar — que é como um teste vira decoração.
	proibidos := []string{
		"time.NewTicker(1 * time.Second)",
		"time.NewTicker(1*time.Second)",
		"time.Tick(1 * time.Second)",
		"time.Tick(1*time.Second)",
	}
	for lineNo, line := range stripComments(string(src)) {
		for _, p := range proibidos {
			if strings.Contains(line, p) {
				t.Fatalf("main.go:%d ressuscitou o laço de 1 segundo: %s", lineNo+1, strings.TrimSpace(line))
			}
		}
	}

	// E o consumidor precisa existir de verdade — senão "não há ticker de 1 s" seria verdade num
	// agente que simplesmente não busca job nenhum.
	code := strings.Join(stripComments(string(src)), "\n")
	for _, obrigatorio := range []string{"go runner.Run(ctx, wake)", "signalWake(wake)", "tr.apply("} {
		if !strings.Contains(code, obrigatorio) {
			t.Fatalf("main.go não contém %q: o laço de consumo não está ligado", obrigatorio)
		}
	}
}

// A conversão da fila do servidor para o consumidor preserva os dois nomes do identificador que o
// servidor devolve (`id` e `jobId`).
func TestConversaoDaFilaAceitaIdEJobId(t *testing.T) {
	got := toRunnerJobs([]pendingJob{
		{Id: "a", PrinterExternalId: "p1", DownloadUrl: "u1"},
		{JobId: "b", PrinterExternalId: "p2", DownloadUrl: "u2"},
		{Id: "c", JobId: "ignorado", PrinterExternalId: "p3", DownloadUrl: "u3"},
	})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("jobs = %d, esperado %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("jobs[%d].ID = %q, esperado %q", i, got[i].ID, want[i])
		}
	}
}
