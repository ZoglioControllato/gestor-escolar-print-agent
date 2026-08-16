package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// PPE-25: existe um cliente único e ele tem deadline. Sem isto, o poll síncrono do agente pendura
// para sempre numa conexão morta (diagnostic.md §3 achado #4).
func TestNewClientTemDeadlineDePontaAPonta(t *testing.T) {
	c := New()
	if got := c.HTTPClient().Timeout; got != RequestTimeout {
		t.Fatalf("Timeout do cliente = %v, esperado %v", got, RequestTimeout)
	}
	if RequestTimeout != 30*time.Second {
		t.Fatalf("RequestTimeout = %v, a spec fixa 30s", RequestTimeout)
	}
	tr, ok := c.HTTPClient().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, esperado *http.Transport", c.HTTPClient().Transport)
	}
	if tr.DialContext == nil {
		t.Fatal("Transport.DialContext nulo: o dial cairia no default sem timeout de conexão")
	}
	if DialTimeout != 10*time.Second {
		t.Fatalf("DialTimeout = %v, a spec fixa 10s", DialTimeout)
	}
}

// PPE-25: URL inválida vira erro tratado. O código anterior fazia `req, _ := http.NewRequest(...)` e
// dereferenciava o nil logo em seguida — este teste é o que mata aquele padrão.
func TestDoComURLInvalidaDevolveErroSemPanico(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Do entrou em pânico com URL inválida: %v", r)
		}
	}()
	c := New()
	resp, err := c.Do(context.Background(), Request{Method: "GET", URL: "http://exemplo\x7f.invalido/x"})
	if err == nil {
		t.Fatalf("esperado erro para URL inválida, veio resposta %+v", resp)
	}
	if resp != nil {
		t.Fatalf("resposta deveria ser nil junto com o erro, veio %+v", resp)
	}
}

// PPE-25: método inválido também sai como erro, não pânico.
func TestDoComMetodoInvalidoDevolveErro(t *testing.T) {
	c := New()
	if _, err := c.Do(context.Background(), Request{Method: "MÉ TODO", URL: "http://127.0.0.1/x"}); err == nil {
		t.Fatal("esperado erro para método inválido")
	}
}

// PPE-25: context nulo é recusado explicitamente — um caminho sem context é um caminho sem
// cancelamento, e o silêncio esconderia exatamente o defeito que esta task fecha.
func TestDoComContextNuloDevolveErro(t *testing.T) {
	c := New()
	//lint:ignore SA1012 o teste existe justamente para provar a recusa
	if _, err := c.Do(nil, Request{URL: "http://127.0.0.1/x"}); !errors.Is(err, ErrNilContext) {
		t.Fatalf("erro = %v, esperado ErrNilContext", err)
	}
}

// PPE-25: servidor que nunca responde não congela o agente — o deadline do context corta.
func TestDoRespeitaDeadlineDoContextContraServidorMudo(t *testing.T) {
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked // nunca responde enquanto o teste roda
	}))
	defer func() {
		close(blocked)
		srv.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := New().Do(ctx, Request{Method: "GET", URL: srv.URL})
	if err == nil {
		t.Fatal("esperado erro de deadline contra servidor mudo")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Do demorou %v: o deadline não cortou", elapsed)
	}
}

// PPE-25: o token vira `Authorization: Bearer` uma vez só, no cliente — nenhuma tela repete a
// montagem do header.
func TestDoEnviaBearerEContentTypeEHeadersExtras(t *testing.T) {
	var got *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		body = make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		w.WriteHeader(204)
	}))
	defer srv.Close()

	_, err := New().Do(context.Background(), Request{
		Method:      "POST",
		URL:         srv.URL,
		Token:       "  tok-123  ",
		ContentType: "application/json",
		Body:        []byte(`{"a":1}`),
		Header:      map[string]string{"If-None-Match": "etag-9"},
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if h := got.Header.Get("Authorization"); h != "Bearer tok-123" {
		t.Fatalf("Authorization = %q, esperado %q", h, "Bearer tok-123")
	}
	if h := got.Header.Get("Content-Type"); h != "application/json" {
		t.Fatalf("Content-Type = %q", h)
	}
	if h := got.Header.Get("If-None-Match"); h != "etag-9" {
		t.Fatalf("If-None-Match = %q", h)
	}
	if string(body) != `{"a":1}` {
		t.Fatalf("corpo = %q", string(body))
	}
}

// PPE-25: sem token, nenhum header de autorização vazio é enviado.
func TestDoSemTokenNaoEnviaAuthorization(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
	}))
	defer srv.Close()

	if _, err := New().Do(context.Background(), Request{URL: srv.URL, Token: "   "}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if _, ok := got.Header["Authorization"]; ok {
		t.Fatalf("Authorization presente sem token: %q", got.Header.Get("Authorization"))
	}
}

// PPE-06: o 429 real de `GET /pending-jobs` traz Retry-After **e** pollIntervalMs; o header vence.
func TestCooldownHonraRetryAfterEPollIntervalMs(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header http.Header
		body   string
		want   time.Duration
	}{
		{
			name:   "429 com Retry-After em segundos",
			status: 429,
			header: http.Header{"Retry-After": []string{"10"}},
			body:   `{"jobs":[],"pollIntervalMs":10000}`,
			want:   10 * time.Second,
		},
		{
			name:   "429 só com pollIntervalMs (header ausente)",
			status: 429,
			header: http.Header{},
			body:   `{"jobs":[],"pollIntervalMs":10000}`,
			want:   10 * time.Second,
		},
		{
			name:   "429 com Retry-After ilegível cai para o corpo",
			status: 429,
			header: http.Header{"Retry-After": []string{"logo-mais"}},
			body:   `{"jobs":[],"pollIntervalMs":2500}`,
			want:   2500 * time.Millisecond,
		},
		{
			name:   "header vence o corpo quando os dois existem e divergem",
			status: 429,
			header: http.Header{"Retry-After": []string{"3"}},
			body:   `{"pollIntervalMs":60000}`,
			want:   3 * time.Second,
		},
		{
			name:   "429 sem nenhum dos dois não inventa espera",
			status: 429,
			header: http.Header{},
			body:   `{"jobs":[]}`,
			want:   0,
		},
		{
			name:   "200 normal não gera cooldown",
			status: 200,
			header: http.Header{},
			body:   `{"jobs":[]}`,
			want:   0,
		},
		{
			name:   "Retry-After fora de 429 (503) também é honrado",
			status: 503,
			header: http.Header{"Retry-After": []string{"7"}},
			body:   ``,
			want:   7 * time.Second,
		},
		{
			name:   "Retry-After negativo não vira espera negativa",
			status: 429,
			header: http.Header{"Retry-After": []string{"-5"}},
			body:   ``,
			want:   0,
		},
		{
			name:   "pollIntervalMs zero é ignorado",
			status: 429,
			header: http.Header{},
			body:   `{"pollIntervalMs":0}`,
			want:   0,
		},
		{
			name:   "corpo não-JSON num 429 não quebra nada",
			status: 429,
			header: http.Header{},
			body:   `<html>rate limited</html>`,
			want:   0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Response{StatusCode: tc.status, Header: tc.header, Body: []byte(tc.body)}
			if got := r.Cooldown(); got != tc.want {
				t.Fatalf("Cooldown() = %v, esperado %v", got, tc.want)
			}
		})
	}
}

// PPE-06: a forma data-absoluta do Retry-After também é aceita.
func TestCooldownAceitaRetryAfterComoData(t *testing.T) {
	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	r := &Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{future}}}
	got := r.Cooldown()
	if got <= 0 || got > 31*time.Second {
		t.Fatalf("Cooldown() = %v, esperado algo em (0, 31s]", got)
	}

	past := time.Now().Add(-30 * time.Second).UTC().Format(http.TimeFormat)
	rPast := &Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{past}}}
	if got := rPast.Cooldown(); got != 0 {
		t.Fatalf("Cooldown() para data no passado = %v, esperado 0", got)
	}
}

// Cooldown num ponteiro nulo é 0, não pânico — o chamador do poll não deve precisar de nil check.
func TestCooldownEmRespostaNulaEZero(t *testing.T) {
	var r *Response
	if got := r.Cooldown(); got != 0 {
		t.Fatalf("Cooldown() = %v em resposta nula", got)
	}
	if r.OK() || r.RateLimited() || r.Unauthorized() {
		t.Fatal("OK/RateLimited/Unauthorized deveriam ser falsos em resposta nula")
	}
}

// PPE-20: Unauthorized só é verdadeiro em 401 — é o sinal que separa "insistir com backoff" (rede
// oscilando) de "parar e decidir" (re-pair único ou Desvinculado); confundi-lo com outro código
// (403, por exemplo, que também é uma negativa mas não a que este fluxo trata) dispararia a reação
// errada.
func TestUnauthorizedSoEVerdadeiroEm401(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, false},
		{http.StatusOK, false},
		{http.StatusTooManyRequests, false},
		{http.StatusInternalServerError, false},
	}
	for _, tc := range cases {
		r := &Response{StatusCode: tc.status}
		if got := r.Unauthorized(); got != tc.want {
			t.Fatalf("Unauthorized() para status %d = %v, esperado %v", tc.status, got, tc.want)
		}
	}
}

// PPE-25: o corpo grande (PDF de job) sai por Open, sem passar pela memória.
func TestOpenDevolveCorpoAbertoEHonraContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("%PDF-1.4 conteudo"))
	}))
	defer srv.Close()

	resp, err := New().Open(context.Background(), Request{URL: srv.URL})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 8)
	n, _ := resp.Body.Read(buf)
	if string(buf[:n]) != "%PDF-1.4" {
		t.Fatalf("primeiros bytes = %q", string(buf[:n]))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New().Open(ctx, Request{URL: srv.URL}); err == nil {
		t.Fatal("Open com context cancelado deveria falhar")
	}
}

// Status: OK() e RateLimited() classificam o que o restante do agente decide em cima.
func TestClassificacaoDeStatus(t *testing.T) {
	if !(&Response{StatusCode: 204}).OK() {
		t.Fatal("204 deveria ser OK")
	}
	if (&Response{StatusCode: 401}).OK() {
		t.Fatal("401 não é OK")
	}
	if !(&Response{StatusCode: 429}).RateLimited() {
		t.Fatal("429 deveria ser RateLimited")
	}
	if (&Response{StatusCode: 500}).RateLimited() {
		t.Fatal("500 não é RateLimited")
	}
}
