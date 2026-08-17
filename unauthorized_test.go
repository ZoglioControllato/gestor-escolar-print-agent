package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"print-agent/internal/events"
)

// resetUnauthorizedState deixa o estado do episódio de 401 num ponto conhecido — o mesmo motivo de
// resetRuntimeConfig existir: runtimeState é global (é o estado do processo), então cada teste que
// mexe nele precisa partir do zero.
func resetUnauthorizedState(t *testing.T) {
	t.Helper()
	runtimeState.unauthorized.Store(false)
	runtimeState.unauthorizedHandling.Store(false)
}

// noSleep substitui unauthorizedSleep para o teste não esperar unauthorizedBackoff (5s) de verdade.
func noSleep(ctx context.Context, d time.Duration) error { return nil }

// ---------------------------------------------------------------------------------------------
// 401/erro de autorização: transportes param, re-pair único, "Desvinculado"
// ---------------------------------------------------------------------------------------------

// Caminho inseguro: sem enrollmentKey salva, handleUnauthorized nunca tenta pair — vai direto para
// Desvinculado. É a prova de que "revogado não volta a pollar": nenhuma chamada ao servidor.
func TestHandleUnauthorizedSemEnrollmentKeyVaiDiretoParaDesvinculado(t *testing.T) {
	resetRuntimeConfig(t, nil)
	resetUnauthorizedState(t)

	var pairCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&pairCalls, 1)
	}))
	defer srv.Close()

	SetRuntimeConfig(&Config{Server: srv.URL, TokenFile: filepath.Join(t.TempDir(), "token.txt")})

	handleUnauthorized(context.Background(), &transport{}, make(chan struct{}, 1))

	if got := atomic.LoadInt32(&pairCalls); got != 0 {
		t.Fatalf("pair foi chamado %d vez(es) sem enrollmentKey salva", got)
	}
	if !IsAgentUnauthorized() {
		t.Fatal("esperava estado Desvinculado")
	}
}

// Com enrollmentKey salva e o servidor aceitando o re-pair: exatamente 1 tentativa, o agente sai do
// estado Desvinculado com o token novo.
func TestHandleUnauthorizedComEnrollmentKeyTentaReRepairUmaVezEResume(t *testing.T) {
	resetRuntimeConfig(t, nil)
	resetUnauthorizedState(t)
	orig := unauthorizedSleep
	unauthorizedSleep = noSleep
	defer func() { unauthorizedSleep = orig }()

	var pairCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == printAgentAPIPrefix+"/pair" {
			atomic.AddInt32(&pairCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"token-novo"}`))
		}
	}))
	defer srv.Close()

	SetRuntimeConfig(&Config{
		Server:        srv.URL,
		Name:          "Agente de Teste",
		EnrollmentKey: "chave-salva",
		TokenFile:     filepath.Join(t.TempDir(), "token.txt"),
	})

	handleUnauthorized(context.Background(), &transport{}, make(chan struct{}, 1))

	if got := atomic.LoadInt32(&pairCalls); got != 1 {
		t.Fatalf("pair foi chamado %d vez(es), esperado exatamente 1", got)
	}
	if IsAgentUnauthorized() {
		t.Fatal("re-pair teve sucesso mas o agente ficou marcado Desvinculado")
	}
	if got := GetAgentToken(); got != "token-novo" {
		t.Fatalf("token após re-pair = %q, esperado o novo token emitido", got)
	}
}

// Chamadas concorrentes (REST e WS sinalizando quase juntas) resultam em **1** tentativa de
// re-pair, não uma por sinal — a prova direta de "exatamente 1 tentativa" do Done-when da task.
func TestHandleUnauthorizedChamadasConcorrentesFazemUmaSoTentativa(t *testing.T) {
	resetRuntimeConfig(t, nil)
	resetUnauthorizedState(t)
	orig := unauthorizedSleep
	unauthorizedSleep = noSleep
	defer func() { unauthorizedSleep = orig }()

	var pairCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == printAgentAPIPrefix+"/pair" {
			atomic.AddInt32(&pairCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"token-novo"}`))
		}
	}))
	defer srv.Close()

	SetRuntimeConfig(&Config{
		Server:        srv.URL,
		Name:          "Agente de Teste",
		EnrollmentKey: "chave-salva",
		TokenFile:     filepath.Join(t.TempDir(), "token.txt"),
	})

	tr := &transport{}
	wake := make(chan struct{}, 1)
	done := make(chan struct{}, 5)
	for i := 0; i < 5; i++ {
		go func() {
			handleUnauthorized(context.Background(), tr, wake)
			done <- struct{}{}
		}()
	}
	for i := 0; i < 5; i++ {
		<-done
	}

	if got := atomic.LoadInt32(&pairCalls); got != 1 {
		t.Fatalf("pair foi chamado %d vez(es) para 5 sinais concorrentes, esperado exatamente 1", got)
	}
}

// Re-pair que falha (servidor fora do ar) também vai para Desvinculado — não fica tentando de novo.
func TestHandleUnauthorizedReRepairFalhaVaiParaDesvinculado(t *testing.T) {
	resetRuntimeConfig(t, nil)
	resetUnauthorizedState(t)
	orig := unauthorizedSleep
	unauthorizedSleep = noSleep
	defer func() { unauthorizedSleep = orig }()

	var pairCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&pairCalls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	SetRuntimeConfig(&Config{
		Server:        srv.URL,
		Name:          "Agente de Teste",
		EnrollmentKey: "chave-salva",
		TokenFile:     filepath.Join(t.TempDir(), "token.txt"),
	})

	handleUnauthorized(context.Background(), &transport{}, make(chan struct{}, 1))

	if got := atomic.LoadInt32(&pairCalls); got != 1 {
		t.Fatalf("pair foi chamado %d vez(es), esperado exatamente 1 mesmo falhando", got)
	}
	if !IsAgentUnauthorized() {
		t.Fatal("re-pair falhou mas o agente não ficou Desvinculado")
	}
}

// handleUnauthorized desliga o transporte WS antes de decidir o resto — "parar os transportes" não
// é opcional nem condicional a ter enrollmentKey.
func TestHandleUnauthorizedDesligaOTransporte(t *testing.T) {
	resetRuntimeConfig(t, nil)
	resetUnauthorizedState(t)

	SetRuntimeConfig(&Config{Server: "http://127.0.0.1:0", TokenFile: filepath.Join(t.TempDir(), "token.txt")})

	tr := &transport{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wake := make(chan struct{}, 1)

	// Liga um cliente qualquer (o host não precisa ser alcançável — só precisamos que tr.client
	// exista antes de handleUnauthorized rodar).
	tr.apply(ctx, events.Config{RealtimeEndpoint: "127.0.0.1:1", HTTPHost: "h", Channel: "/print/a/b", Token: "tok"}, true, wake)
	tr.mu.Lock()
	hadClient := tr.client != nil
	tr.mu.Unlock()
	if !hadClient {
		t.Fatal("pré-condição: o transporte deveria ter um cliente antes de handleUnauthorized")
	}

	handleUnauthorized(ctx, tr, wake) // sem enrollmentKey: vai direto para Desvinculado

	tr.mu.Lock()
	stillClient := tr.client != nil
	tr.mu.Unlock()
	if stillClient {
		t.Fatal("o transporte continuou ligado depois de handleUnauthorized")
	}
}

// ---------------------------------------------------------------------------------------------
// "Revogado não volta a pollar": os produtores de tráfego calam com IsAgentUnauthorized()
// ---------------------------------------------------------------------------------------------

// fetchPendingJobs devolve errAgentUnauthorized quando o servidor recusa com 401 — é o sinal que o
// closure de Fetch em run() usa para disparar handleUnauthorized.
func TestFetchPendingJobsDevolveErroDeAutorizacaoEm401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.Server = srv.URL
	_, _, err := fetchPendingJobs(context.Background(), cfg, "tok-revogado")
	if err != errAgentUnauthorized {
		t.Fatalf("erro = %v, esperado errAgentUnauthorized", err)
	}
}

// reportStatus e sendHeartbeat também propagam o mesmo sinal — os 3 pontos de entrada REST que o
// achado de 401 precisa cobrir.
func TestReportStatusESendHeartbeatDevolvemErroDeAutorizacaoEm401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	cfg := testConfig()
	cfg.Server = srv.URL

	if err := reportStatus(context.Background(), cfg, "tok", "job-1", "completed", ""); err != errAgentUnauthorized {
		t.Fatalf("reportStatus: erro = %v, esperado errAgentUnauthorized", err)
	}
	if err := sendHeartbeat(context.Background(), cfg, "tok", "poll"); err != errAgentUnauthorized {
		t.Fatalf("sendHeartbeat: erro = %v, esperado errAgentUnauthorized", err)
	}
}

// syncDeviceConfig também propaga o sinal — é o 4º ponto de entrada REST (device-config).
func TestSyncDeviceConfigDevolveErroDeAutorizacaoEm401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	resetRuntimeConfig(t, &Config{Server: srv.URL, TokenFile: filepath.Join(t.TempDir(), "token.txt")})

	if err := syncDeviceConfig(context.Background(), "tok"); err != errAgentUnauthorized {
		t.Fatalf("erro = %v, esperado errAgentUnauthorized", err)
	}
}

// Estado Desvinculado é visível e reversível: o painel (manual) e handleUnauthorized (automático)
// são os dois únicos caminhos de saída.
func TestIsAgentUnauthorizedRefleteOEstado(t *testing.T) {
	resetUnauthorizedState(t)
	if IsAgentUnauthorized() {
		t.Fatal("estado inicial deveria ser false")
	}
	SetAgentUnauthorized(true)
	if !IsAgentUnauthorized() {
		t.Fatal("SetAgentUnauthorized(true) não refletiu em IsAgentUnauthorized()")
	}
	SetAgentUnauthorized(false)
	if IsAgentUnauthorized() {
		t.Fatal("SetAgentUnauthorized(false) não refletiu em IsAgentUnauthorized()")
	}
}
