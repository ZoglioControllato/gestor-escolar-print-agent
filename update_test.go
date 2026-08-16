package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildTarGz cria um tar.gz em memória contendo entries (name → conteúdo).
func buildTarGz(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range entries {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0755,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write content: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func TestExtractAgentBinaryFromTarGz_Found(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "pkg.tar.gz")
	body := buildTarGz(t, map[string]string{
		"gestor-escolar-linux-arm64-2.0.2/install.sh":     "#!/bin/sh\necho install",
		"gestor-escolar-linux-arm64-2.0.2/gestor-escolar": "BINARY-CONTENT-v2.0.2",
		"gestor-escolar-linux-arm64-2.0.2/uninstall.sh":   "#!/bin/sh\necho uninstall",
	})
	if err := os.WriteFile(src, body, 0644); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(dir, "out-binary")
	if err := extractAgentBinaryFromTarGz(src, dest); err != nil {
		t.Fatalf("extract: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "BINARY-CONTENT-v2.0.2" {
		t.Fatalf("conteúdo extraído errado: %q", got)
	}

	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0100 == 0 {
		t.Fatalf("binário extraído sem bit executável: mode=%v", info.Mode())
	}
}

func TestExtractAgentBinaryFromTarGz_Missing(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "pkg.tar.gz")
	body := buildTarGz(t, map[string]string{
		"gestor-escolar-linux-arm64-2.0.2/install.sh": "#!/bin/sh",
	})
	if err := os.WriteFile(src, body, 0644); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(dir, "out-binary")
	err := extractAgentBinaryFromTarGz(src, dest)
	if err == nil {
		t.Fatal("esperava erro quando binário ausente, mas extract retornou nil")
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Fatal("dest não deveria ter sido criado quando binário ausente")
	}
}

func TestSwapBinary_Success(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "agent")
	if err := os.WriteFile(exePath, []byte("OLD"), 0755); err != nil {
		t.Fatal(err)
	}
	newBin := filepath.Join(dir, "agent.new")
	if err := os.WriteFile(newBin, []byte("NEW"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := swapBinary(exePath, newBin); err != nil {
		t.Fatalf("swap: %v", err)
	}

	got, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW" {
		t.Fatalf("binário atual deveria ser NEW, got %q", got)
	}

	backup, err := os.ReadFile(exePath + ".old")
	if err != nil {
		t.Fatalf("backup .old ausente: %v", err)
	}
	if string(backup) != "OLD" {
		t.Fatalf("backup deveria conter OLD, got %q", backup)
	}
}

// countOrphanUpdateTemps conta arquivos com o prefixo que downloadToTemp usa em os.CreateTemp — é
// o que prova, sem depender do caminho exato devolvido em erro, que uma recusa não deixa lixo.
func countOrphanUpdateTemps(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatalf("ler TempDir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "gestor-escolar-update-") {
			n++
		}
	}
	return n
}

// PPE-18: manifest sem sha256 é o caminho inseguro — este teste prova que ele é recusado, não só que
// o caminho feliz (com hash certo) funciona.
func TestDownloadToTemp_SemSHA256Recusa(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("conteudo-do-artefato"))
	}))
	defer srv.Close()

	before := countOrphanUpdateTemps(t)
	cfg := &Config{Server: srv.URL}
	artifact := versionArtifact{URL: srv.URL + "/artefato.tar.gz"} // sem SHA256

	path, err := downloadToTemp(context.Background(), cfg, artifact, ".tar.gz")
	if err == nil {
		t.Fatal("esperava recusa por manifest sem sha256, downloadToTemp devolveu sucesso")
	}
	if !errors.Is(err, ErrMissingSHA256) {
		t.Fatalf("erro = %v, esperado ErrMissingSHA256", err)
	}
	if path != "" {
		t.Fatalf("path deveria ser vazio numa recusa, veio %q", path)
	}
	if after := countOrphanUpdateTemps(t); after != before {
		t.Fatalf("recusa deixou %d arquivo(s) temporário(s) órfão(s) (antes=%d, depois=%d)", after-before, before, after)
	}
}

// PPE-18: hash incorreto também é recusa — comportamento que já existia antes desta task (o "already
// existed" do Done-when), mas sem teste até agora.
func TestDownloadToTemp_SHA256IncorretoRecusa(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("conteudo-do-artefato"))
	}))
	defer srv.Close()

	before := countOrphanUpdateTemps(t)
	cfg := &Config{Server: srv.URL}
	artifact := versionArtifact{
		URL:    srv.URL + "/artefato.tar.gz",
		SHA256: strings.Repeat("0", 64), // hash válido em forma, errado em conteúdo
	}

	path, err := downloadToTemp(context.Background(), cfg, artifact, ".tar.gz")
	if err == nil {
		t.Fatal("esperava recusa por sha256 incorreto, downloadToTemp devolveu sucesso")
	}
	if !strings.Contains(err.Error(), "SHA-256 inválido") {
		t.Fatalf("erro = %v, esperado menção a SHA-256 inválido", err)
	}
	if path != "" {
		t.Fatalf("path deveria ser vazio numa recusa, veio %q", path)
	}
	if after := countOrphanUpdateTemps(t); after != before {
		t.Fatalf("recusa deixou %d arquivo(s) temporário(s) órfão(s)", after-before)
	}
}

// Caminho feliz: hash certo aplica normalmente. Sem isto, um teto que só provasse as duas recusas
// acima deixaria passar despercebido um bug que recusasse hash **correto** também.
func TestDownloadToTemp_SHA256CorretoAceita(t *testing.T) {
	body := "conteudo-do-artefato"
	sum := sha256Hex(t, body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	cfg := &Config{Server: srv.URL}
	artifact := versionArtifact{URL: srv.URL + "/artefato.tar.gz", SHA256: sum}

	path, err := downloadToTemp(context.Background(), cfg, artifact, ".tar.gz")
	if err != nil {
		t.Fatalf("downloadToTemp com hash correto falhou: %v", err)
	}
	defer os.Remove(path)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("conteúdo baixado = %q, esperado %q", got, body)
	}
}

func sha256Hex(t *testing.T, s string) string {
	t.Helper()
	h := sha256.New()
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

// PPE-18: o `.old` de um swap anterior é removido no startup seguinte — nunca se acumula.
func TestRemoveStaleUpdateBackup_RemoveOArquivo(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "agent")
	old := exe + ".old"
	if err := os.WriteFile(old, []byte("STALE"), 0644); err != nil {
		t.Fatal(err)
	}

	removeStaleUpdateBackup(exe)

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf(".old ainda existe depois do startup (err=%v)", err)
	}
}

// Ausência de `.old` não é erro nem cria nada — startup normal, sem update pendente.
func TestRemoveStaleUpdateBackup_SemOldNaoFalhaNemCriaNada(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "agent")

	removeStaleUpdateBackup(exe) // não deve panicar

	if _, err := os.Stat(exe + ".old"); !os.IsNotExist(err) {
		t.Fatal("arquivo .old inesperado depois de removeStaleUpdateBackup sem .old prévio")
	}
}

// exePath vazio (falha de resolveExecutablePath) é um no-op seguro, não um panic.
func TestRemoveStaleUpdateBackup_ExePathVazioNaoPanica(t *testing.T) {
	removeStaleUpdateBackup("")
}

func TestResolveArtifactURL(t *testing.T) {
	cases := []struct{ base, artifact, want string }{
		{"https://app.gestor.com", "/print-agent/x.tar.gz", "https://app.gestor.com/print-agent/x.tar.gz"},
		{"https://app.gestor.com/", "/print-agent/x.tar.gz", "https://app.gestor.com/print-agent/x.tar.gz"},
		{"https://app.gestor.com", "https://cdn.example/x.tar.gz", "https://cdn.example/x.tar.gz"},
		{"https://app.gestor.com", "print-agent/x.tar.gz", "https://app.gestor.com/print-agent/x.tar.gz"},
		{"https://app.gestor.com", "", ""},
	}
	for _, c := range cases {
		got := resolveArtifactURL(c.base, c.artifact)
		if got != c.want {
			t.Errorf("resolveArtifactURL(%q,%q)=%q, want %q", c.base, c.artifact, got, c.want)
		}
	}
}
