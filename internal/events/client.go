// Package events implementa o transporte push do agente: um cliente WebSocket do AWS AppSync
// Events API que troca o poll de 1 segundo por um sinal de "há trabalho".
//
// O que **não** está aqui, de propósito: o job. O evento é magro (`{type, jobId, ts}`) e serve só
// para acordar o consumidor, que busca a fila por `GET /pending-jobs` como sempre. Isso mantém um
// único código de consumo para push e para poll, e é o que torna o push uma otimização de latência
// em vez de uma condição de correção — se o WebSocket nunca conectar, a impressão continua saindo.
package events

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Subprotocolo do Events API. Vai junto com `header-<base64url(authJson)>` no handshake.
const wsSubprotocol = "aws-appsync-event-ws"

// Caminho do endpoint de tempo real. O device-config entrega **host**, não URL.
const realtimePath = "/event/realtime"

const (
	// handshakeTimeout limita dial + connection_ack. Curto de propósito: um handshake que não
	// fecha é indistinguível de firewall, e o lugar certo de esperar é o backoff, não a conexão.
	handshakeTimeout = 15 * time.Second

	// defaultConnectionTimeout é o watchdog de `ka` usado quando o servidor não manda
	// `connectionTimeoutMs` no ack. 5 min é o default documentado do Events API.
	defaultConnectionTimeout = 5 * time.Minute

	// minConnectionTimeout evita que um `connectionTimeoutMs` absurdamente pequeno vindo do
	// servidor transforme o watchdog num laço de reconexão.
	minConnectionTimeout = 50 * time.Millisecond
)

// errUnauthorized marca a negativa do authorizer (token revogado, device rejeitado, drift do
// espelho). Não é motivo para insistir no WebSocket: vira sinal para o agente decidir re-pair ou
// "Desvinculado".
var errUnauthorized = errors.New("events: autorização recusada")

// Status é o estado do transporte, lido pelo seletor de intervalo do poll de fallback (10 s com
// o WebSocket fora, 300 s com ele assinado).
type Status int32

const (
	// StatusDisconnected — sem WebSocket. O poll é o transporte, não a rede de segurança.
	StatusDisconnected Status = iota
	// StatusConnecting — handshake em curso; ainda não há garantia de entrega de evento.
	StatusConnecting
	// StatusSubscribed — assinado no canal. Só aqui o poll pode relaxar para reconciliação.
	StatusSubscribed
)

func (s Status) String() string {
	switch s {
	case StatusSubscribed:
		return "subscribed"
	case StatusConnecting:
		return "connecting"
	default:
		return "disconnected"
	}
}

// Config é o bloco `events` do `GET /print-agent/device-config` mais o token do device.
//
// `RealtimeEndpoint` e `HTTPHost` são **hosts** (ex.: `events-dev.pedagogicoonline.com.br`), não
// URLs — é o que o servidor publica (`print-agent-device-config.int.test.ts:33-34`) e o que o
// firewall da escola libera.
type Config struct {
	RealtimeEndpoint string
	HTTPHost         string
	Channel          string
	Token            string
}

// Valid reporta se há informação suficiente para tentar o push. Config incompleta significa "opere
// em poll" — nunca "tente contra um host vazio e insista".
func (c Config) Valid() bool {
	return strings.TrimSpace(c.RealtimeEndpoint) != "" &&
		strings.TrimSpace(c.HTTPHost) != "" &&
		strings.TrimSpace(c.Channel) != "" &&
		strings.TrimSpace(c.Token) != ""
}

// realtimeURL monta `wss://<host>/event/realtime`. Aceita host com esquema já embutido, que é como
// o teste aponta o cliente para um `httptest.Server`.
func (c Config) realtimeURL() string {
	host := strings.TrimRight(strings.TrimSpace(c.RealtimeEndpoint), "/")
	switch {
	case strings.HasPrefix(host, "ws://"), strings.HasPrefix(host, "wss://"):
	case strings.HasPrefix(host, "http://"):
		host = "ws://" + strings.TrimPrefix(host, "http://")
	case strings.HasPrefix(host, "https://"):
		host = "wss://" + strings.TrimPrefix(host, "https://")
	default:
		host = "wss://" + host
	}
	return host + realtimePath
}

// authJSON é o objeto de autorização do Events API: o mesmo no handshake e no subscribe.
func (c Config) authJSON() map[string]string {
	httpHost := strings.TrimSpace(c.HTTPHost)
	httpHost = strings.TrimPrefix(strings.TrimPrefix(httpHost, "https://"), "http://")
	httpHost = strings.TrimRight(httpHost, "/")
	return map[string]string{
		"host":          httpHost,
		"Authorization": strings.TrimSpace(c.Token),
	}
}

// authSubprotocol codifica a autorização no subprotocol `header-<base64url-sem-padding>`, que é
// como o Events API recebe cabeçalho num handshake de WebSocket (o navegador não deixa mandar
// header próprio, então o protocolo carrega a autorização por aqui).
func (c Config) authSubprotocol() string {
	raw, _ := json.Marshal(c.authJSON())
	return "header-" + base64.RawURLEncoding.EncodeToString(raw)
}

// Event é o evento magro publicado pelo backend no canal do device.
type Event struct {
	Type  string `json:"type"`
	JobID string `json:"jobId"`
	TS    string `json:"ts"`
}

// Option customiza o cliente. As opções existem para o teste controlar tempo e sorteio; produção
// usa os defaults.
type Option func(*Client)

// WithSleep injeta a espera do backoff. O teste registra as durações em vez de dormi-las — dormir
// de verdade num teste de backoff é lento e, pior, vira sincronização implícita.
func WithSleep(f func(context.Context, time.Duration) error) Option {
	return func(c *Client) { c.sleep = f }
}

// WithRand injeta o sorteio do full jitter, para o teste fixar os extremos da distribuição.
func WithRand(f func(n int64) int64) Option {
	return func(c *Client) { c.randN = f }
}

// WithOnEvent registra um observador dos eventos decodificados (telemetria: `jobId` correlaciona o
// push com o job criado). É também como o teste verifica a **dupla decodificação**.
func WithOnEvent(f func(Event)) Option {
	return func(c *Client) { c.onEvent = f }
}

// WithHTTPClient reusa o `*http.Client` do agente no handshake, em vez de abrir um transporte novo.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// WithLogf injeta o log. O pacote não conhece o `log()` do agente (que é do pacote main).
func WithLogf(f func(string, ...any)) Option {
	return func(c *Client) { c.logf = f }
}

// Client mantém uma assinatura viva no canal do device, reconectando sozinho.
type Client struct {
	cfg Config

	// wake tem capacidade 1: é o coalescing. N eventos numa rajada viram **um** sinal, e um sinal
	// pendente absorve os seguintes sem fila — que é exatamente o que se quer, porque o fetch
	// devolve a fila inteira, não um job.
	wake chan struct{}

	// unauthorized também tem capacidade 1: a negativa é um fato, não uma contagem.
	unauthorized chan struct{}

	status atomic.Int32

	sleep      func(context.Context, time.Duration) error
	randN      func(n int64) int64
	onEvent    func(Event)
	httpClient *http.Client
	logf       func(string, ...any)
	now        func() time.Time
}

// New cria o cliente. Nada de rede acontece até Run.
func New(cfg Config, opts ...Option) *Client {
	c := &Client{
		cfg:          cfg,
		wake:         make(chan struct{}, 1),
		unauthorized: make(chan struct{}, 1),
		sleep:        sleepCtx,
		randN:        defaultRand,
		onEvent:      func(Event) {},
		logf:         func(string, ...any) {},
		now:          time.Now,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Wake emite um sinal sempre que há motivo para buscar a fila: evento recebido **ou**
// `subscribe_success` (a drenagem de reconexão — a janela em que o WebSocket estava fora é
// justamente quando um evento pode ter sido perdido).
func (c *Client) Wake() <-chan struct{} { return c.wake }

// Unauthorized emite quando o authorizer recusa. Consumido pelo tratamento de 401.
func (c *Client) Unauthorized() <-chan struct{} { return c.unauthorized }

// Status devolve o estado atual do transporte.
func (c *Client) Status() Status { return Status(c.status.Load()) }

func (c *Client) setStatus(s Status) { c.status.Store(int32(s)) }

func (c *Client) signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default: // já há sinal pendente: coalescido
	}
}

// Run mantém a assinatura viva até `ctx` ser cancelado. Bloqueia; o chamador roda numa goroutine.
func (c *Client) Run(ctx context.Context) {
	if !c.cfg.Valid() {
		c.logf("[EVENTS] configuração incompleta — operando só em poll")
		return
	}
	failures := 0
	for ctx.Err() == nil {
		if failures > 0 {
			delay := backoffDelay(failures, c.randN)
			c.logf("[EVENTS] reconectando em %v (falha %d)", delay.Round(time.Millisecond), failures)
			if err := c.sleep(ctx, delay); err != nil {
				return
			}
		}
		startedAt := c.now()
		err := c.session(ctx)
		connectedFor := c.now().Sub(startedAt)
		c.setStatus(StatusDisconnected)

		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errUnauthorized) {
			// Não adianta insistir: o token é que está recusado. Quem decide o que fazer (re-pair
			// único ou "Desvinculado") é o agente, não o transporte.
			c.logf("[EVENTS] autorização recusada: %v", err)
			c.signal(c.unauthorized)
			return
		}
		if err != nil {
			c.logf("[EVENTS] sessão encerrada: %v", err)
		}
		failures = nextFailureCount(failures, connectedFor)
	}
}

// session abre uma conexão, assina o canal e lê até a conexão morrer.
func (c *Client) session(ctx context.Context) error {
	c.setStatus(StatusConnecting)

	dialCtx, cancelDial := context.WithTimeout(ctx, handshakeTimeout)
	conn, resp, err := websocket.Dial(dialCtx, c.cfg.realtimeURL(), &websocket.DialOptions{
		Subprotocols: []string{wsSubprotocol, c.cfg.authSubprotocol()},
		HTTPClient:   c.httpClient,
	})
	cancelDial()
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return fmt.Errorf("%w: handshake HTTP %d", errUnauthorized, resp.StatusCode)
		}
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()

	if err := writeJSON(ctx, conn, map[string]any{"type": "connection_init"}, handshakeTimeout); err != nil {
		return fmt.Errorf("connection_init: %w", err)
	}

	connTimeout, err := c.awaitAck(ctx, conn)
	if err != nil {
		return err
	}

	// subscribeID tem que ser um UUID — exigência do serviço, não estética. Medido ao vivo em
	// devel (2026-08-12): com o caminho do canal como id, o Events API responde
	// `{"errorType":"UnsupportedOperation"}` logo após o ack e derruba a sessão — o CloudWatch
	// mostrava 27 ConnectSuccess / 0 ActiveSubscriptions e o agente preso no poll de fallback.
	// O MESMO frame com UUID recebe subscribe_success. Era a "inferência de protocolo do
	// subscribe" que o runbook mandava confirmar com wscat: confirmada, e estava errada.
	subscribeID, err := randomUUID()
	if err != nil {
		return fmt.Errorf("subscribe id: %w", err)
	}
	if err := writeJSON(ctx, conn, map[string]any{
		"type":    "subscribe",
		"id":      subscribeID,
		"channel": c.cfg.Channel,
		// O objeto de autorização vai **também** no subscribe: é ele que alimenta o
		// `authorizationToken` da operação `EVENT_SUBSCRIBE` do authorizer, e é o EVENT_SUBSCRIBE
		// que carrega o isolamento multi-tenant inteiro (design.md § authorizer, achado 1 de T6 —
		// `channel` é `null` no EVENT_CONNECT). Mandar de novo é inócuo se o serviço herdar a
		// autorização da conexão; **omitir** seria um subscribe recusado em produção.
		"authorization": c.cfg.authJSON(),
	}, handshakeTimeout); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	// O watchdog de `ka` é o deadline de cada leitura: o Events API promete um keep-alive dentro de
	// `connectionTimeoutMs`, então silêncio maior que isso **é** a conexão morta. Um timer separado
	// diria a mesma coisa com mais peças móveis.
	for {
		msg, err := readMessage(ctx, conn, connTimeout)
		if err != nil {
			return err
		}
		switch msg.Type {
		case "ka":
			// Só de ter chegado, o watchdog já foi renovado.
		case "subscribe_success":
			c.setStatus(StatusSubscribed)
			c.logf("[EVENTS] assinado em %s", c.cfg.Channel)
			// Drenagem: a janela sem assinatura é onde um evento pode ter se perdido.
			c.signal(c.wake)
		case "data":
			c.handleData(msg.Event)
		case "subscribe_error", "connection_error", "error":
			if isAuthError(msg.Errors) {
				return fmt.Errorf("%w: %s", errUnauthorized, msg.Type)
			}
			return fmt.Errorf("%s: %s", msg.Type, string(msg.Errors))
		default:
			// Mensagem desconhecida não derruba a sessão: o protocolo pode ganhar tipos novos, e
			// derrubar por isso trocaria uma conexão saudável por um laço de reconexão.
			c.logf("[EVENTS] mensagem ignorada: %s", msg.Type)
		}
	}
}

// awaitAck espera o `connection_ack` e devolve o watchdog de `ka` que ele anuncia.
func (c *Client) awaitAck(ctx context.Context, conn *websocket.Conn) (time.Duration, error) {
	msg, err := readMessage(ctx, conn, handshakeTimeout)
	if err != nil {
		return 0, fmt.Errorf("connection_ack: %w", err)
	}
	switch msg.Type {
	case "connection_ack":
		timeout := time.Duration(msg.ConnectionTimeoutMs) * time.Millisecond
		if timeout < minConnectionTimeout {
			timeout = defaultConnectionTimeout
		}
		return timeout, nil
	case "connection_error", "error":
		if isAuthError(msg.Errors) {
			return 0, fmt.Errorf("%w: %s", errUnauthorized, msg.Type)
		}
		return 0, fmt.Errorf("%s: %s", msg.Type, string(msg.Errors))
	default:
		return 0, fmt.Errorf("esperado connection_ack, veio %q", msg.Type)
	}
}

// handleData faz a **dupla decodificação**: o Events API entrega o evento como uma *string* JSON
// dentro do campo `event` de uma mensagem JSON (o backend serializa duas vezes ao publicar,
// `print-events.ts` § publishBody). Ler só a camada de fora devolveria uma string, não o evento.
func (c *Client) handleData(raw string) {
	// Acordar é o que importa, e vale mesmo se o payload vier ilegível: o fetch é que decide se há
	// trabalho. Trocar isso por "só acorda se decodificar" faria um evento com campo novo virar uma
	// impressão que não sai.
	c.signal(c.wake)

	var ev Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		c.logf("[EVENTS] evento ilegível (acordando mesmo assim): %v", err)
		return
	}
	c.logf("[EVENTS] evento %s job=%s", ev.Type, ev.JobID)
	c.onEvent(ev)
}

// wsMessage é o envelope do protocolo. Campos não usados pelo agente ficam de fora de propósito.
type wsMessage struct {
	Type                string          `json:"type"`
	ID                  string          `json:"id,omitempty"`
	Event               string          `json:"event,omitempty"`
	ConnectionTimeoutMs int64           `json:"connectionTimeoutMs,omitempty"`
	Errors              json.RawMessage `json:"errors,omitempty"`
}

func readMessage(ctx context.Context, conn *websocket.Conn, timeout time.Duration) (wsMessage, error) {
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		return wsMessage{}, err
	}
	var msg wsMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return wsMessage{}, fmt.Errorf("mensagem inválida: %w", err)
	}
	return msg, nil
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any, timeout time.Duration) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, data)
}

// isAuthError separa "o authorizer recusou" de "a conexão caiu".
//
// A distinção importa porque as duas reações são opostas: queda pede reconexão com backoff;
// recusa pede **parar** e tratar como 401. Insistir contra um token revogado é o laço quente que
// o agente proíbe.
func isAuthError(errs json.RawMessage) bool {
	lower := strings.ToLower(string(errs))
	return strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "forbidden") ||
		strings.Contains(lower, "accessdenied")
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

// randomUUID gera um UUID v4 com crypto/rand — sem dependência nova. O formato é contrato do
// subscribe do Events API (ver comentário no call site em run()).
func randomUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
