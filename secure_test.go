package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------------------------
// Modo 0600 em arquivo sensível (token/config)
//
// O ambiente de teste roda em Linux/CI: aqui é onde o bit Unix é o mecanismo real de proteção — a
// ACL do Windows (secure_windows.go, via icacls) não é verificável neste ambiente e fica só na
// revisão de código + do .iss.
// ---------------------------------------------------------------------------------------------

// saveConfig grava config.json em 0600, nunca 0644.
//
// configDataDirOverride isola o teste num diretório temporário — sem ele, este teste exercitaria
// configDataDir() contra o caminho real de produção (/var/lib/gestor-escolar em Linux), que exige
// root e falha por permissão em runners CI sem privilégio.
func TestSaveConfigGravaConfigJsonEm0600(t *testing.T) {
	configDataDirOverride = t.TempDir()
	t.Cleanup(func() { configDataDirOverride = "" })

	cfg := testConfig()
	cfg.EnrollmentKey = "chave-de-teste"
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	path := filepath.Join(configDataDir(), "config.json")

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("config.json não foi criado: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("modo de config.json = %o, esperado 0600 (era 0644 antes)", perm)
	}
}

// ensureDataFiles cria token.txt em 0600 (o placeholder inicial, antes de qualquer pair).
func TestEnsureDataFilesCriaTokenTxtEm0600(t *testing.T) {
	configDataDirOverride = t.TempDir()
	t.Cleanup(func() { configDataDirOverride = "" })

	dir := t.TempDir()
	cfg := &Config{TokenFile: filepath.Join(dir, "token.txt")}

	if err := ensureDataFiles(cfg); err != nil {
		t.Fatalf("ensureDataFiles: %v", err)
	}

	info, err := os.Stat(cfg.TokenFile)
	if err != nil {
		t.Fatalf("token.txt não foi criado: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("modo de token.txt = %o, esperado 0600 (era 0644 antes)", perm)
	}
}

// pair() grava o token recebido do servidor em 0600.
func TestPairGravaTokenEm0600(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"tok-abc-123"}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := &Config{Server: srv.URL, Name: "Agente de Teste", TokenFile: filepath.Join(dir, "token.txt")}

	if _, err := pair(context.Background(), cfg); err != nil {
		t.Fatalf("pair: %v", err)
	}
	info, err := os.Stat(cfg.TokenFile)
	if err != nil {
		t.Fatalf("token.txt não foi criado pelo pair: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("modo de token.txt (via pair) = %o, esperado 0600", perm)
	}
}

// syncDeviceConfig não fala mais com /device-config/ack nem
// grava arquivo de etag — mesmo quando o corpo traz um `etag` (um servidor antigo em cache, ou um
// futuro descuidado, poderia mandar o campo de volta; a prova é que isso não ressuscita nenhum dos
// dois efeitos colaterais). Só o `GET /device-config` deveria ser chamado — qualquer outro path
// falha o teste.
func TestSyncDeviceConfigNaoFalaMaisComAckNemGravaEtag(t *testing.T) {
	configDataDirOverride = t.TempDir()
	t.Cleanup(func() { configDataDirOverride = "" })

	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"printSettings":"noscale","etag":"etag-v1"}`))
	}))
	defer srv.Close()

	resetRuntimeConfig(t, &Config{Server: srv.URL, TokenFile: filepath.Join(t.TempDir(), "token.txt")})

	etagPath := filepath.Join(configDataDir(), ".device-config.etag")

	if err := syncDeviceConfig(context.Background(), "tok"); err != nil {
		t.Fatalf("syncDeviceConfig: %v", err)
	}
	if GetRuntimeConfig().PrintSettings != "noscale" {
		t.Fatalf("política não aplicada: PrintSettings = %q", GetRuntimeConfig().PrintSettings)
	}
	if len(gotPaths) != 1 || gotPaths[0] != "/print-agent/device-config" {
		t.Fatalf("chamadas ao servidor = %v, esperado só GET /print-agent/device-config (sem ack)", gotPaths)
	}
	if _, err := os.Stat(etagPath); !os.IsNotExist(err) {
		t.Fatalf("etag não deveria mais ser gravado (%s), stat err = %v", etagPath, err)
	}
}

// restrictFilePermissions é no-op fora do Windows (secure_stub.go) — nunca falha, nunca muda o
// conteúdo do arquivo.
func TestRestrictFilePermissionsNaoWindowsENoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "arquivo.txt")
	if err := os.WriteFile(path, []byte("conteudo"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restrictFilePermissions(path); err != nil {
		t.Fatalf("restrictFilePermissions (stub) deveria ser sempre nil, veio %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "conteudo" {
		t.Fatal("restrictFilePermissions (stub) alterou o conteúdo do arquivo")
	}
}

// ---------------------------------------------------------------------------------------------
// Varredura de temp órfão (startup + reaper periódico, sem goroutine por job)
// ---------------------------------------------------------------------------------------------

// sweepOrphanTempFiles remove só o que é mais velho que maxAge; arquivo recente sobrevive.
func TestSweepOrphanTempFilesRemoveSoOsMaisVelhosQueMaxAge(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	velho := filepath.Join(dir, "velho.pdf")
	novo := filepath.Join(dir, "novo.pdf")
	if err := os.WriteFile(velho, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(novo, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	// Retroage o mtime do "velho" em 10 min — sem tocar o relógio real (now é injetado à parte).
	tenMinAgo := now.Add(-10 * time.Minute)
	if err := os.Chtimes(velho, tenMinAgo, tenMinAgo); err != nil {
		t.Fatal(err)
	}

	removed := sweepOrphanTempFiles(dir, 3*time.Minute, now)
	if removed != 1 {
		t.Fatalf("removidos = %d, esperado 1", removed)
	}
	if _, err := os.Stat(velho); !os.IsNotExist(err) {
		t.Fatal("arquivo velho deveria ter sido removido")
	}
	if _, err := os.Stat(novo); err != nil {
		t.Fatal("arquivo novo não deveria ter sido removido")
	}
}

// maxAge zero (o caso do startup) remove tudo — o agente não estava rodando, nada é "recente" de
// verdade.
func TestSweepOrphanTempFilesComMaxAgeZeroRemoveTudo(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(dir, "job-"+string(rune('a'+i))+".pdf"), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	removed := sweepOrphanTempFiles(dir, 0, time.Now())
	if removed != 3 {
		t.Fatalf("removidos = %d, esperado 3", removed)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("%d arquivo(s) sobrevivente(s) ao sweep com maxAge zero", len(entries))
	}
}

// Diretório inexistente (temp/ ainda não criado) é um no-op seguro, não um erro fatal.
func TestSweepOrphanTempFilesDiretorioInexistenteNaoFalha(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nao-existe")
	if removed := sweepOrphanTempFiles(dir, 0, time.Now()); removed != 0 {
		t.Fatalf("removidos = %d, esperado 0 num diretório inexistente", removed)
	}
}

// Subdiretório dentro do temp (não deveria existir na prática) é ignorado, não removido — a função
// só apaga arquivo regular.
func TestSweepOrphanTempFilesIgnoraSubdiretorio(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	removed := sweepOrphanTempFiles(dir, 0, time.Now())
	if removed != 0 {
		t.Fatalf("removidos = %d, esperado 0 (só há um subdiretório)", removed)
	}
	if _, err := os.Stat(sub); err != nil {
		t.Fatal("subdiretório foi removido")
	}
}

// Nenhuma goroutine dedicada de 3 min por job — a versão executável do "goroutine de 3 min
// por job só para apagar arquivo".
func TestSemGoroutineDe3MinutosPorJob(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("ler main.go: %v", err)
	}
	code := strings.Join(stripComments(string(src)), "\n")
	proibidos := []string{
		"time.Sleep(3 * time.Minute)",
		"time.Sleep(3*time.Minute)",
	}
	for _, p := range proibidos {
		if strings.Contains(code, p) {
			t.Fatalf("main.go ainda tem uma espera de 3 min encadeada a um job: %q", p)
		}
	}
	// E o reaper compartilhado precisa existir de verdade — senão "sem goroutine por job" seria
	// verdade num agente que também não limpa mais nada.
	for _, obrigatorio := range []string{"sweepOrphanTempFiles(tempDir()", "time.NewTicker(tempSweepInterval)"} {
		if !strings.Contains(code, obrigatorio) {
			t.Fatalf("main.go não contém %q: o reaper periódico não está ligado", obrigatorio)
		}
	}
}
