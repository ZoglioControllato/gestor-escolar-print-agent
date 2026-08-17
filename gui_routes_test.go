package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------------------------
// Sessão do painel local: header exigido, Origin/Host validados
// ---------------------------------------------------------------------------------------------

// Caminho inseguro #1: sem o header de sessão → 403, e o handler interno nunca roda.
func TestRequireGUISessionRecusaSemHeader(t *testing.T) {
	called := false
	h := requireGUISession("segredo-do-boot", func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:17345/api/status", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, esperado 403", rec.Code)
	}
	if called {
		t.Fatal("handler interno rodou sem o header de sessão")
	}
}

// Header presente mas errado também é 403 — não basta mandar *algum* valor.
func TestRequireGUISessionRecusaHeaderErrado(t *testing.T) {
	h := requireGUISession("segredo-do-boot", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:17345/api/status", nil)
	req.Header.Set(GUISessionHeader, "chute-qualquer")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, esperado 403", rec.Code)
	}
}

// Caminho inseguro #2: Origin estranha → 403, mesmo com o header de sessão certo.
func TestRequireGUISessionRecusaOrigemEstranha(t *testing.T) {
	h := requireGUISession("segredo-do-boot", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:17345/api/enroll", nil)
	req.Header.Set(GUISessionHeader, "segredo-do-boot")
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, esperado 403 (Origin estranha)", rec.Code)
	}
}

// Host diferente de 127.0.0.1:17345 (ex.: rebinding de DNS) também recusa.
func TestRequireGUISessionRecusaHostErrado(t *testing.T) {
	h := requireGUISession("segredo-do-boot", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "http://evil.example/api/status", nil)
	req.Header.Set(GUISessionHeader, "segredo-do-boot")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, esperado 403 (Host errado)", rec.Code)
	}
}

// Caminho feliz: header certo, Host certo, sem Origin (cliente HTTP comum) → passa.
func TestRequireGUISessionAceitaComHeaderCerto(t *testing.T) {
	h := requireGUISession("segredo-do-boot", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:17345/api/status", nil)
	req.Header.Set(GUISessionHeader, "segredo-do-boot")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, esperado 200", rec.Code)
	}
}

// Origin que bate com o próprio painel também passa (é o caso normal de fetch() a partir da página).
func TestRequireGUISessionAceitaOrigemDoProprioPainel(t *testing.T) {
	h := requireGUISession("segredo-do-boot", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:17345/api/status", nil)
	req.Header.Set(GUISessionHeader, "segredo-do-boot")
	req.Header.Set("Origin", "http://127.0.0.1:17345")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, esperado 200", rec.Code)
	}
}

// ---------------------------------------------------------------------------------------------
// Funções puras de apoio
// ---------------------------------------------------------------------------------------------

func TestNewGUISessionTokenGeraValoresLongosEDiferentes(t *testing.T) {
	a, err := newGUISessionToken()
	if err != nil {
		t.Fatalf("newGUISessionToken: %v", err)
	}
	b, err := newGUISessionToken()
	if err != nil {
		t.Fatalf("newGUISessionToken: %v", err)
	}
	if a == "" || b == "" {
		t.Fatal("token vazio")
	}
	if a == b {
		t.Fatal("duas chamadas geraram o mesmo token — não é aleatório de verdade")
	}
	if len(a) != 64 { // 32 bytes em hex = 64 chars
		t.Fatalf("len(token) = %d, esperado 64 (32 bytes em hex)", len(a))
	}
}

func TestValidGUIRequestExigeHostFixo(t *testing.T) {
	cases := []struct {
		nome   string
		host   string
		origin string
		want   bool
	}{
		{"host certo, sem origin", "127.0.0.1:17345", "", true},
		{"host certo, origin do próprio painel", "127.0.0.1:17345", "http://127.0.0.1:17345", true},
		{"host certo, origin estranha", "127.0.0.1:17345", "http://evil.example", false},
		{"host errado", "evil.example", "", false},
		{"host vazio", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.nome, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/api/status", nil)
			if tc.host == "" {
				req.Host = "" // httptest.NewRequest preenche Host a partir da URL; força vazio aqui
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if got := validGUIRequest(req); got != tc.want {
				t.Fatalf("validGUIRequest = %v, esperado %v", got, tc.want)
			}
		})
	}
}

func TestMaskTokenNuncaDevolveOValorIntegro(t *testing.T) {
	cases := []struct {
		nome  string
		token string
	}{
		{"token longo (uuid v4)", "550e8400-e29b-41d4-a716-446655440000"},
		{"token curto", "abc123"},
	}
	for _, tc := range cases {
		t.Run(tc.nome, func(t *testing.T) {
			got := maskToken(tc.token)
			if got == tc.token {
				t.Fatalf("maskToken devolveu o valor íntegro: %q", got)
			}
			if got == "" {
				t.Fatal("máscara vazia para token não-vazio")
			}
		})
	}
	if got := maskToken(""); got != "" {
		t.Fatalf("maskToken(\"\") = %q, esperado vazio", got)
	}
	longo := "550e8400-e29b-41d4-a716-446655440000"
	got := maskToken(longo)
	if !strings.Contains(got, "…") {
		t.Fatalf("máscara de token longo deveria conter reticências: %q", got)
	}
	if !strings.HasPrefix(got, longo[:4]) || !strings.HasSuffix(got, longo[len(longo)-4:]) {
		t.Fatalf("máscara = %q, esperado prefixo/sufixo de 4 chars do original", got)
	}
}

func TestRenderGUIHTMLInjetaOTokenESomeComOPlaceholder(t *testing.T) {
	html := renderGUIHTML("token-de-sessao-123")
	if !strings.Contains(html, "token-de-sessao-123") {
		t.Fatal("renderGUIHTML não injetou o token de sessão")
	}
	if strings.Contains(html, guiSessionPlaceholder) {
		t.Fatal("placeholder sobreviveu ao render — o JS leria o literal %%AGENT_SESSION%%")
	}
	if !strings.Contains(html, "X-Agent-Session") {
		t.Fatal("página não manda o header de sessão de volta em nenhuma chamada")
	}
}

func TestMaskEnrollmentKeyNuncaDevolveOValorIntegro(t *testing.T) {
	cfg := &Config{EnrollmentKey: "pm_1234567890abcdef"}
	got := maskEnrollmentKey(cfg)
	if got == cfg.EnrollmentKey {
		t.Fatal("maskEnrollmentKey devolveu o valor íntegro")
	}
	if maskEnrollmentKey(nil) != "" {
		t.Fatal("maskEnrollmentKey(nil) deveria ser vazio")
	}
}

// ---------------------------------------------------------------------------------------------
// Fim a fim: mux real, servido na porta fixa do painel (a mesma que validGUIRequest exige)
// ---------------------------------------------------------------------------------------------

// Caminho inseguro #3 provado fim a fim: GET /api/status sem o header → 403; com o header → 200 e
// o token nunca aparece íntegro no corpo.
func TestPainelE2E_StatusExigeSessaoEMascaraOToken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:17345")
	if err != nil {
		t.Skipf("porta 17345 indisponível neste ambiente de teste: %v", err)
	}

	const tokenIntegro = "550e8400-e29b-41d4-a716-446655440000"
	resetRuntimeConfig(t, testConfig())
	SetTrayToken(tokenIntegro)
	t.Cleanup(func() { SetTrayToken("") })

	mux := http.NewServeMux()
	registerGUIRoutes(mux, testConfig(), "sessao-do-boot")
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	base := "http://127.0.0.1:17345"

	respSemHeader, err := http.Get(base + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	_ = respSemHeader.Body.Close()
	if respSemHeader.StatusCode != http.StatusForbidden {
		t.Fatalf("sem header: status = %d, esperado 403", respSemHeader.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, base+"/api/status", nil)
	req.Header.Set(GUISessionHeader, "sessao-do-boot")
	respComHeader, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer respComHeader.Body.Close()
	if respComHeader.StatusCode != http.StatusOK {
		t.Fatalf("com header: status = %d, esperado 200", respComHeader.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(respComHeader.Body).Decode(&body); err != nil {
		t.Fatalf("decodificar resposta: %v", err)
	}
	tok, _ := body["token"].(string)
	if tok == tokenIntegro {
		t.Fatal("resposta de /api/status devolveu o token íntegro")
	}
	if !strings.Contains(tok, "…") {
		t.Fatalf("token na resposta não parece mascarado: %q", tok)
	}
}

// ---------------------------------------------------------------------------------------------
// O estado "Desvinculado" precisa chegar ao usuário via /api/status (achado 2 do gate
// spec-driven-eval, 20260810T023120Z): o mecanismo de revogação já funcionava internamente
// (IsAgentUnauthorized()), mas a resposta de status não tinha o campo — este teste prova que agora tem.
// ---------------------------------------------------------------------------------------------

// statusResponse chama /api/status via mux (sem precisar de um listener real na porta fixa) e
// decodifica o corpo — helper compartilhado pelos dois testes de estado abaixo.
func statusResponse(t *testing.T) map[string]any {
	t.Helper()
	mux := http.NewServeMux()
	registerGUIRoutes(mux, testConfig(), "sessao-do-boot")

	req := httptest.NewRequest(http.MethodGet, "http://"+guiAllowedHost()+"/api/status", nil)
	req.Header.Set(GUISessionHeader, "sessao-do-boot")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/status = %d, esperado 200", rec.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decodificar /api/status: %v", err)
	}
	return body
}

// Estado normal (nunca revogado): unauthorized sai false, e o texto de status não é "Desvinculado".
func TestApiStatusUnauthorizedFalseNoEstadoNormal(t *testing.T) {
	resetRuntimeConfig(t, testConfig())
	resetUnauthorizedState(t)

	body := statusResponse(t)

	got, ok := body["unauthorized"].(bool)
	if !ok {
		t.Fatalf("/api/status não devolveu o campo \"unauthorized\" (ou não é bool): %#v", body["unauthorized"])
	}
	if got {
		t.Fatal("unauthorized = true no estado normal (IsAgentUnauthorized() nunca foi marcado)")
	}
	if body["status"] == "Desvinculado" {
		t.Fatal("status = \"Desvinculado\" no estado normal")
	}
}

// Estado revogado (IsAgentUnauthorized() == true): unauthorized sai true e o texto de status vira
// "Desvinculado" — antes desta correção nenhum dos dois campos existia na resposta.
func TestApiStatusExpoeUnauthorizedQuandoDesvinculado(t *testing.T) {
	resetRuntimeConfig(t, testConfig())
	resetUnauthorizedState(t)
	SetAgentUnauthorized(true)
	t.Cleanup(func() { resetUnauthorizedState(t) })

	body := statusResponse(t)

	got, ok := body["unauthorized"].(bool)
	if !ok {
		t.Fatalf("/api/status não devolveu o campo \"unauthorized\" (ou não é bool): %#v", body["unauthorized"])
	}
	if !got {
		t.Fatal("unauthorized = false com IsAgentUnauthorized() == true")
	}
	if body["status"] != "Desvinculado" {
		t.Fatalf("status = %q, esperado \"Desvinculado\"", body["status"])
	}
}

// O painel HTML lê d.unauthorized e reflete no dot/hint — prova estática de que o JS foi atualizado
// junto (a asserção de comportamento real do DOM ficaria a cargo de um teste de browser, fora do
// escopo Go; aqui travamos que a página **contém** a lógica, não que ela deixou de ser escrita).
func TestGuiHTMLReflecteEstadoDesvinculado(t *testing.T) {
	if !strings.Contains(guiHTML, "d.unauthorized") {
		t.Fatal("guiHTML não lê o campo unauthorized da resposta de /api/status")
	}
	if !strings.Contains(guiHTML, "unauthorized-hint") {
		t.Fatal("guiHTML não tem elemento para explicar o estado Desvinculado ao usuário")
	}
}

// "/" continua acessível sem o header — é a própria página que entrega o token ao navegador.
func TestPainelE2E_RaizNaoExigeSessaoEEntregaOToken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:17345")
	if err != nil {
		t.Skipf("porta 17345 indisponível neste ambiente de teste: %v", err)
	}
	mux := http.NewServeMux()
	registerGUIRoutes(mux, testConfig(), "sessao-do-boot")
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	resp, err := http.Get("http://127.0.0.1:17345/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, esperado 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "sessao-do-boot") {
		t.Fatal("página raiz não entregou o token de sessão")
	}
}
