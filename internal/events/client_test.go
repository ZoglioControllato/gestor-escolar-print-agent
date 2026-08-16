package events

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// runClient sobe o cliente numa goroutine e garante que ele para no fim do teste.
func runClient(t *testing.T, c *Client) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run não retornou depois do cancelamento")
		}
	})
	return cancel
}

// waitWake espera um sinal de acordar, falhando em vez de pendurar.
func waitWake(t *testing.T, c *Client, within time.Duration) {
	t.Helper()
	select {
	case <-c.Wake():
	case <-time.After(within):
		t.Fatalf("nenhum sinal de wake em %v", within)
	}
}

// PPE-02: o handshake carrega os dois subprotocols que o Events API exige, e o segundo decodifica
// para o objeto de autorização com host e token do device. Errar isto é um handshake recusado em
// produção — e o `-s` do wscat de `amplify/README.md` monta exatamente esta string.
func TestHandshakeOfereceSubprotocolsComAutorizacao(t *testing.T) {
	f := newFakeEventsAPI(t)
	c := New(testCfg(f))
	runClient(t, c)

	f.waitConnection(t, 3*time.Second)
	waitWake(t, c, 3*time.Second) // subscribe_success → drenagem

	hs := f.handshakes()
	if len(hs) != 1 {
		t.Fatalf("handshakes = %d, esperado 1", len(hs))
	}
	if len(hs[0].subprotocols) != 2 {
		t.Fatalf("subprotocols = %v, esperado 2", hs[0].subprotocols)
	}
	if hs[0].subprotocols[0] != wsSubprotocol {
		t.Fatalf("primeiro subprotocol = %q, esperado %q", hs[0].subprotocols[0], wsSubprotocol)
	}
	if !strings.HasPrefix(hs[0].subprotocols[1], "header-") {
		t.Fatalf("segundo subprotocol = %q, esperado prefixo header-", hs[0].subprotocols[1])
	}
	if !hs[0].authDecoded {
		t.Fatal("o subprotocol header- não decodificou como base64url de JSON")
	}
	if hs[0].authToken != "device-token-secreto" {
		t.Fatalf("Authorization = %q", hs[0].authToken)
	}
	// O `host` do authJSON é o **HTTP** host (PPE-34), nunca o realtime nem o host do teste.
	if hs[0].authHost != "events-dev.pedagogicoonline.com.br" {
		t.Fatalf("host = %q", hs[0].authHost)
	}
}

// PPE-03: o subscribe pede exatamente o canal do device e leva a autorização junto — é o
// `EVENT_SUBSCRIBE` que carrega o isolamento multi-tenant inteiro (o `channel` é `null` no
// `EVENT_CONNECT`, achado 1 de T6). Um subscribe sem `authorization` seria recusado.
func TestSubscribePedeOCanalDoDeviceComAutorizacao(t *testing.T) {
	f := newFakeEventsAPI(t)
	c := New(testCfg(f))
	runClient(t, c)

	f.waitConnection(t, 3*time.Second)
	waitWake(t, c, 3*time.Second)

	sub := f.subscribeMessage()
	if sub == nil {
		t.Fatal("nenhuma mensagem subscribe recebida")
	}
	if sub["channel"] != "/print/conta-1/device-1" {
		t.Fatalf("channel = %v", sub["channel"])
	}
	if sub["id"] == nil || sub["id"] == "" {
		t.Fatal("subscribe sem id")
	}
	// Regressão do bug de 2026-08-12 (devel, medido ao vivo): o id era o caminho do canal e o
	// Events API REAL respondia `UnsupportedOperation`, derrubando a sessão logo após o ack —
	// 27 ConnectSuccess / 0 ActiveSubscriptions no CloudWatch. O serviço exige UUID no id.
	subID, _ := sub["id"].(string)
	uuidV4 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidV4.MatchString(subID) {
		t.Fatalf("subscribe id não é UUID v4: %q", subID)
	}
	auth, ok := sub["authorization"].(map[string]any)
	if !ok {
		t.Fatalf("subscribe sem objeto authorization: %v", sub)
	}
	if auth["Authorization"] != "device-token-secreto" {
		t.Fatalf("authorization.Authorization = %v", auth["Authorization"])
	}
	if auth["host"] != "events-dev.pedagogicoonline.com.br" {
		t.Fatalf("authorization.host = %v", auth["host"])
	}
}

// PPE-06: `subscribe_success` dispara um poll imediato de drenagem, e o estado vira Subscribed —
// que é o que faz o poll de fallback relaxar de 10 s para 300 s.
func TestSubscribeSuccessAcordaEMudaStatus(t *testing.T) {
	f := newFakeEventsAPI(t)
	c := New(testCfg(f))
	if c.Status() != StatusDisconnected {
		t.Fatalf("status inicial = %v", c.Status())
	}
	runClient(t, c)

	f.waitConnection(t, 3*time.Second)
	waitWake(t, c, 3*time.Second)

	if got := c.Status(); got != StatusSubscribed {
		t.Fatalf("status = %v, esperado subscribed", got)
	}
}

// PPE-02: o evento chega **duplamente codificado** (`{"type":"data","event":"<json string>"}`),
// porque é assim que o backend publica (`print-events.ts` § publishBody). Ler só a camada de fora
// devolveria uma string, e o `jobId` da telemetria se perderia.
func TestEventoComDuplaDecodificacaoAcordaEEntregaJobId(t *testing.T) {
	f := newFakeEventsAPI(t)

	recebidos := make(chan Event, 4)
	c := New(testCfg(f), WithOnEvent(func(e Event) { recebidos <- e }))
	runClient(t, c)

	s := f.waitConnection(t, 3*time.Second)
	waitWake(t, c, 3*time.Second) // drenagem do subscribe

	interno, _ := json.Marshal(Event{Type: "job.created", JobID: "job-abc", TS: "2026-08-09T12:00:00Z"})
	if err := s.send(context.Background(), map[string]any{
		"type":  "data",
		"id":    "sub",
		"event": string(interno),
	}); err != nil {
		t.Fatalf("enviar data: %v", err)
	}

	waitWake(t, c, 3*time.Second)

	select {
	case ev := <-recebidos:
		if ev.Type != "job.created" || ev.JobID != "job-abc" {
			t.Fatalf("evento decodificado = %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("evento não foi decodificado")
	}
}

// Edge case da spec (rajada): N eventos em <2 s viram **um** fetch. O canal de capacidade 1 é o
// coalescing; sem ele, 20 jobs criados juntos virariam 20 chamadas a `/pending-jobs`, cada uma
// devolvendo a mesma fila — e 19 delas bateriam no rate limit.
func TestEventosEmRajadaCoalescemEmUmUnicoWake(t *testing.T) {
	f := newFakeEventsAPI(t)

	const rajada = 20
	entregues := make(chan struct{}, rajada)
	c := New(testCfg(f), WithOnEvent(func(Event) { entregues <- struct{}{} }))
	runClient(t, c)

	s := f.waitConnection(t, 3*time.Second)
	waitWake(t, c, 3*time.Second) // consome a drenagem para partir de zero

	interno, _ := json.Marshal(Event{Type: "job.created", JobID: "j"})
	for i := 0; i < rajada; i++ {
		if err := s.send(context.Background(), map[string]any{"type": "data", "event": string(interno)}); err != nil {
			t.Fatalf("enviar data %d: %v", i, err)
		}
	}
	// Sincroniza pelos callbacks, não por sleep: quando os 20 chegaram, o wake já está no estado
	// final que o teste quer inspecionar.
	for i := 0; i < rajada; i++ {
		select {
		case <-entregues:
		case <-time.After(5 * time.Second):
			t.Fatalf("só %d de %d eventos chegaram", i, rajada)
		}
	}

	if got := len(c.Wake()); got != 1 {
		t.Fatalf("wake pendente = %d, esperado exatamente 1 (coalescing)", got)
	}
	<-c.Wake()
	select {
	case <-c.Wake():
		t.Fatal("havia mais de um sinal enfileirado: o canal não é de capacidade 1")
	default:
	}
}

// Edge case da spec: sem `ka` dentro do `connectionTimeoutMs`, o agente fecha e reconecta. Sem esse
// watchdog uma conexão morta em silêncio (NAT que expira, firewall que engole) deixaria o agente
// achando que está assinado — e nada imprimiria até o poll de reconciliação de 300 s.
func TestSemKaDentroDoTimeoutOClienteReconecta(t *testing.T) {
	f := newFakeEventsAPI(t)
	f.ackTimeoutMs = 120 // watchdog curtíssimo; o servidor não manda ka nenhum

	c := New(testCfg(f), WithSleep(func(context.Context, time.Duration) error { return nil }))
	runClient(t, c)

	f.waitConnection(t, 3*time.Second)
	// A segunda conexão só existe se o watchdog derrubou a primeira.
	f.waitConnection(t, 5*time.Second)
}

// Contrapositivo do teste acima: **com** `ka` fluindo, o cliente não reconecta. Sem esta metade, um
// watchdog quebrado (que derrubasse sempre) passaria no teste anterior.
func TestComKaFluindoAConexaoSobrevive(t *testing.T) {
	f := newFakeEventsAPI(t)
	f.ackTimeoutMs = 300

	pararKa := make(chan struct{})
	f.onSubscribe = func(s *session) {
		_ = s.send(context.Background(), map[string]any{"type": "subscribe_success", "id": "sub"})
		go func() {
			tk := time.NewTicker(60 * time.Millisecond)
			defer tk.Stop()
			for {
				select {
				case <-pararKa:
					return
				case <-tk.C:
					if err := s.send(context.Background(), map[string]any{"type": "ka"}); err != nil {
						return
					}
				}
			}
		}()
	}
	t.Cleanup(func() { close(pararKa) })

	c := New(testCfg(f), WithSleep(func(context.Context, time.Duration) error { return nil }))
	runClient(t, c)

	f.waitConnection(t, 3*time.Second)
	waitWake(t, c, 3*time.Second)

	// Janela de ~5 watchdogs: se o `ka` não renovasse o deadline, haveria reconexão aqui.
	select {
	case <-f.connected:
		t.Fatal("reconectou apesar do ka: o keep-alive não está renovando o watchdog")
	case <-time.After(1500 * time.Millisecond):
	}
	if got := c.Status(); got != StatusSubscribed {
		t.Fatalf("status = %v depois de 1,5 s de ka", got)
	}
}

// PPE-20/PPE-05: erro de autorização não é queda de rede. Insistir contra um token revogado é o
// laço quente que a spec proíbe — o cliente para e emite o sinal que o agente consome como 401.
func TestErroDeAutorizacaoParaOClienteEEmiteSinal(t *testing.T) {
	f := newFakeEventsAPI(t)
	f.onConnectionInit = func(s *session) bool {
		_ = s.send(context.Background(), map[string]any{
			"type":   "connection_error",
			"errors": []map[string]string{{"errorType": "UnauthorizedException"}},
		})
		return false
	}

	var tentativasDeSleep int32
	var mu sync.Mutex
	c := New(testCfg(f), WithSleep(func(context.Context, time.Duration) error {
		mu.Lock()
		tentativasDeSleep++
		mu.Unlock()
		return nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	select {
	case <-c.Unauthorized():
	case <-time.After(5 * time.Second):
		t.Fatal("nenhum sinal de autorização recusada")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run não parou depois da recusa: laço quente de retry")
	}
	if c.Status() != StatusDisconnected {
		t.Fatalf("status = %v depois da recusa", c.Status())
	}
	mu.Lock()
	defer mu.Unlock()
	if tentativasDeSleep != 0 {
		t.Fatalf("houve %d backoff(s): recusa de autorização não deve virar retry", tentativasDeSleep)
	}
}

// Queda comum (sem sinal de autorização) **é** motivo de reconexão — a metade oposta do teste
// acima. Sem ela, um cliente que parasse em toda queda passaria lá e quebraria a feature.
func TestQuedaComumReconectaComBackoff(t *testing.T) {
	f := newFakeEventsAPI(t)
	f.onSubscribe = func(s *session) {
		_ = s.conn.Close(1011, "queda simulada")
	}

	atrasos := make(chan time.Duration, 8)
	c := New(testCfg(f), WithSleep(func(_ context.Context, d time.Duration) error {
		select {
		case atrasos <- d:
		default:
		}
		return nil
	}))
	runClient(t, c)

	f.waitConnection(t, 3*time.Second)
	f.waitConnection(t, 5*time.Second)

	select {
	case d := <-atrasos:
		if d < 0 || d > MaxDelay {
			t.Fatalf("atraso fora de [0, %v]: %v", MaxDelay, d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reconectou sem passar pelo backoff")
	}
	select {
	case <-c.Unauthorized():
		t.Fatal("queda comum não pode ser confundida com recusa de autorização")
	default:
	}
}

// Config incompleta (kill-switch, bloco `events` ausente) não vira tentativa de conexão contra host
// vazio: o cliente simplesmente não roda, e o agente opera em poll (PPE-28).
func TestConfigIncompletaNaoTentaConectar(t *testing.T) {
	cases := map[string]Config{
		"tudo vazio":       {},
		"sem canal":        {RealtimeEndpoint: "h", HTTPHost: "h", Token: "t"},
		"sem token":        {RealtimeEndpoint: "h", HTTPHost: "h", Channel: "/print/a/b"},
		"sem realtime":     {HTTPHost: "h", Channel: "/print/a/b", Token: "t"},
		"só espaço branco": {RealtimeEndpoint: "  ", HTTPHost: " ", Channel: " ", Token: " "},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if cfg.Valid() {
				t.Fatal("Valid() devolveu true para config incompleta")
			}
			c := New(cfg, WithSleep(func(context.Context, time.Duration) error {
				t.Error("houve backoff: o cliente tentou conectar com config incompleta")
				return nil
			}))
			done := make(chan struct{})
			go func() { c.Run(context.Background()); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("Run não retornou com config incompleta")
			}
		})
	}
}

// O cancelamento do context raiz para o transporte — é o que o graceful shutdown (T27) vai usar.
func TestCancelamentoDoContextParaOCliente(t *testing.T) {
	f := newFakeEventsAPI(t)
	c := New(testCfg(f))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	f.waitConnection(t, 3*time.Second)
	waitWake(t, c, 3*time.Second)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run não retornou depois do cancelamento")
	}
}

// A URL de tempo real é montada a partir do **host** que o device-config publica.
func TestRealtimeURLMontaWssSobreOHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"events-dev.pedagogicoonline.com.br", "wss://events-dev.pedagogicoonline.com.br/event/realtime"},
		{"events.pedagogicoonline.com.br/", "wss://events.pedagogicoonline.com.br/event/realtime"},
		{"https://events.exemplo.test", "wss://events.exemplo.test/event/realtime"},
		{"http://127.0.0.1:8080", "ws://127.0.0.1:8080/event/realtime"},
		{"wss://ja.com.esquema", "wss://ja.com.esquema/event/realtime"},
	}
	for _, tc := range cases {
		got := Config{RealtimeEndpoint: tc.in}.realtimeURL()
		if got != tc.want {
			t.Errorf("realtimeURL(%q) = %q, esperado %q", tc.in, got, tc.want)
		}
	}
}

// O `host` da autorização é sempre um host puro: o servidor real assina/valida sobre ele, e um
// esquema sobrando é um handshake recusado que não diz por quê.
func TestAuthJSONUsaHostPuro(t *testing.T) {
	auth := Config{HTTPHost: "https://events.exemplo.test/", Token: "  t  "}.authJSON()
	if auth["host"] != "events.exemplo.test" {
		t.Fatalf("host = %q", auth["host"])
	}
	if auth["Authorization"] != "t" {
		t.Fatalf("Authorization = %q", auth["Authorization"])
	}

	sub := Config{HTTPHost: "h", Token: "t"}.authSubprotocol()
	if strings.ContainsAny(sub, "+/=") {
		t.Fatalf("subprotocol %q usa base64 padrão: precisa ser base64url sem padding", sub)
	}
}

// isAuthError separa recusa de autorização de queda — a decisão que define parar × reconectar.
func TestIsAuthErrorClassificaAsDuasFamilias(t *testing.T) {
	recusa := []string{
		`[{"errorType":"UnauthorizedException"}]`,
		`[{"errorType":"Forbidden"}]`,
		`[{"message":"AccessDeniedException"}]`,
		`[{"errorType":"unauthorized"}]`,
	}
	for _, e := range recusa {
		if !isAuthError(json.RawMessage(e)) {
			t.Errorf("isAuthError(%s) = false, esperado true", e)
		}
	}
	queda := []string{
		`[{"errorType":"InternalFailure"}]`,
		`[{"errorType":"LimitExceededException"}]`,
		``,
		`null`,
	}
	for _, e := range queda {
		if isAuthError(json.RawMessage(e)) {
			t.Errorf("isAuthError(%s) = true, esperado false", e)
		}
	}
}

// errUnauthorized precisa continuar identificável por errors.Is — é como Run distingue as duas
// reações opostas.
func TestErroDeAutorizacaoEEnvelopavel(t *testing.T) {
	err := errors.New("x")
	if errors.Is(err, errUnauthorized) {
		t.Fatal("erro qualquer casou com errUnauthorized")
	}
	wrapped := errors.Join(errUnauthorized, err)
	if !errors.Is(wrapped, errUnauthorized) {
		t.Fatal("errUnauthorized deixou de ser identificável depois de envelopado")
	}
}
