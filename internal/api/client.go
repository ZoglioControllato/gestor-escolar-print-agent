// Package api concentra toda a saída HTTP do agente numa porta só.
//
// Motivo (PPE-25, diagnostic.md §3 achado #4): antes desta feature o agente falava HTTP por
// `http.DefaultClient`, que **não tem timeout algum** — uma conexão pendurada no poll síncrono
// congelava o agente para sempre, e a máquina só voltava a imprimir com reinício manual. Três call
// sites ainda descartavam o erro de `http.NewRequest` (`req, _ :=`), o que transforma uma URL
// inválida num nil deref e derruba o processo.
//
// Aqui há um único `*http.Client` com deadline em todas as camadas e um `Do` que só aceita
// `context.Context`. Não existe caminho para chamar HTTP sem deadline: quem quiser falar com o
// servidor passa por este pacote.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// RequestTimeout é o teto de ponta a ponta de uma requisição (conexão + envio + resposta).
	// 30 s é generoso para o maior corpo que o agente troca (o PDF de um job) e curto o bastante
	// para que um servidor mudo nunca vire um agente congelado.
	RequestTimeout = 30 * time.Second

	// DialTimeout limita só o estabelecimento da conexão TCP. Separado do teto total porque é o
	// sintoma mais comum na rede de escola: firewall que engole o SYN sem responder.
	DialTimeout = 10 * time.Second

	// MaxResponseBody limita o que `Do` traz para a memória. Respostas do agente são JSON pequeno;
	// corpo grande (o PDF) vai por `Open`, que devolve o stream sem bufferizar.
	MaxResponseBody = 8 << 20 // 8 MiB
)

// Client é a única porta HTTP do agente.
type Client struct {
	hc *http.Client
}

// New devolve o cliente compartilhado do agente, com deadline em todas as camadas.
func New() *Client {
	return &Client{hc: &http.Client{
		Timeout: RequestTimeout,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   DialTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}}
}

// NewWithHTTPClient embrulha um `*http.Client` já pronto. Existe para o teste poder observar o
// transporte; produção usa New.
func NewWithHTTPClient(hc *http.Client) *Client {
	if hc == nil {
		return New()
	}
	return &Client{hc: hc}
}

// HTTPClient expõe o cliente subjacente para quem precisa dele inteiro (ex.: o handshake do
// WebSocket, que faz o próprio upgrade).
func (c *Client) HTTPClient() *http.Client { return c.hc }

// Request descreve uma chamada do agente. `Token`, quando não vazio, vira `Authorization: Bearer`.
type Request struct {
	Method      string
	URL         string
	Token       string
	ContentType string
	Body        []byte
	Header      map[string]string
}

// Response é a resposta já lida. O corpo vem em memória porque toda resposta que passa por `Do` é
// JSON de controle; download de arquivo usa Open.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// ErrNilContext é devolvido quando o chamador esquece o context. Falhar aqui é melhor que assumir
// `context.Background()` em silêncio: um caminho sem context é um caminho sem cancelamento.
var ErrNilContext = errors.New("api: context nulo")

func (c *Client) newRequest(ctx context.Context, r Request) (*http.Request, error) {
	if ctx == nil {
		return nil, ErrNilContext
	}
	method := r.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if len(r.Body) > 0 {
		body = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.URL, body)
	if err != nil {
		return nil, err
	}
	if token := strings.TrimSpace(r.Token); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if r.ContentType != "" {
		req.Header.Set("Content-Type", r.ContentType)
	}
	for k, v := range r.Header {
		req.Header.Set(k, v)
	}
	return req, nil
}

// Do executa a requisição e lê a resposta inteira. Nunca entra em pânico: URL inválida, context
// nulo e context cancelado saem como erro.
func (c *Client) Do(ctx context.Context, r Request) (*Response, error) {
	req, err := c.newRequest(ctx, r)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBody))
	if err != nil {
		return nil, err
	}
	return &Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: data}, nil
}

// Open executa a requisição e devolve a resposta com o corpo **aberto** — para download de arquivo,
// que não deve passar pela memória. O chamador fecha `resp.Body`.
func (c *Client) Open(ctx context.Context, r Request) (*http.Response, error) {
	req, err := c.newRequest(ctx, r)
	if err != nil {
		return nil, err
	}
	return c.hc.Do(req)
}

// OK reporta se a resposta é 2xx.
func (r *Response) OK() bool { return r != nil && r.StatusCode >= 200 && r.StatusCode < 300 }

// RateLimited reporta se o servidor recusou por rate limit.
func (r *Response) RateLimited() bool { return r != nil && r.StatusCode == http.StatusTooManyRequests }

// Unauthorized reporta se o servidor recusou o token (PPE-20): device revogado, rejeitado ou
// removido. É o sinal que separa "insistir com backoff" (rede oscilando) de "parar e decidir"
// (re-pair único ou estado Desvinculado) — insistir contra um token que não vai voltar a ser aceito
// é o loop quente que a AC proíbe.
func (r *Response) Unauthorized() bool { return r != nil && r.StatusCode == http.StatusUnauthorized }

// Cooldown devolve por quanto tempo o servidor pediu silêncio (PPE-06).
//
// `GET /print-agent/pending-jobs` responde ao rate limit com **os dois** sinais
// (`backend/api/src/routes/print-agent.ts:266-267`): o header `Retry-After` em segundos e
// `pollIntervalMs` no corpo. O header tem precedência porque é o padrão HTTP e o único que um
// intermediário (CloudFront) sabe reescrever; o corpo é o fallback.
//
// Devolve 0 quando não há pedido de espera — nunca um default inventado, porque um cooldown que o
// servidor não pediu atrasaria impressão legítima.
func (r *Response) Cooldown() time.Duration {
	if r == nil {
		return 0
	}
	if v := strings.TrimSpace(r.Header.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			if secs <= 0 {
				return 0
			}
			return time.Duration(secs) * time.Second
		}
		// Forma alternativa do RFC 9110: data absoluta.
		if t, err := http.ParseTime(v); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
			return 0
		}
	}
	if r.StatusCode == http.StatusTooManyRequests {
		var body struct {
			PollIntervalMs int64 `json:"pollIntervalMs"`
		}
		if json.Unmarshal(r.Body, &body) == nil && body.PollIntervalMs > 0 {
			return time.Duration(body.PollIntervalMs) * time.Millisecond
		}
	}
	return 0
}
