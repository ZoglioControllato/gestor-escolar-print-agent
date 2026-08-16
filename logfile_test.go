package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A rotação promove o arquivo cheio para `.1` e recomeça vazio — o teto vale para o PAR, não
// para o crescimento sem fim que motivou o teto (frota em escola, disco pequeno).
func TestFileLogSinkRotacionaPorTamanho(t *testing.T) {
	dir := t.TempDir()
	s := newFileLogSink(dir, 100)
	if s == nil {
		t.Fatal("sink não abriu em diretório gravável")
	}

	linha := strings.Repeat("x", 40) + "\n" // 41 bytes
	s.write(linha)
	s.write(linha) // 82 — ainda sob o teto
	s.write(linha) // estouraria 123 → rotaciona antes

	backup, err := os.ReadFile(filepath.Join(dir, logFileBackup))
	if err != nil {
		t.Fatalf("backup ausente pós-rotação: %v", err)
	}
	if got := strings.Count(string(backup), "\n"); got != 2 {
		t.Fatalf("backup com %d linhas, esperado 2", got)
	}
	atual, err := os.ReadFile(filepath.Join(dir, logFileName))
	if err != nil {
		t.Fatalf("arquivo atual ausente pós-rotação: %v", err)
	}
	if got := strings.Count(string(atual), "\n"); got != 1 {
		t.Fatalf("arquivo atual com %d linhas, esperado 1", got)
	}
}

// Segunda rotação SUBSTITUI o backup anterior — é o que limita o disco a ~2× o teto.
func TestFileLogSinkSegundaRotacaoSubstituiBackup(t *testing.T) {
	dir := t.TempDir()
	s := newFileLogSink(dir, 50)
	if s == nil {
		t.Fatal("sink não abriu")
	}
	for i := 0; i < 6; i++ {
		s.write(strings.Repeat("y", 30) + "\n")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		nomes := make([]string, 0, len(entries))
		for _, e := range entries {
			nomes = append(nomes, e.Name())
		}
		t.Fatalf("esperado exatamente {agent.log, agent.log.1}, veio %v", nomes)
	}
}

// logf é chamado de várias goroutines (transporte, jobs, updater) — o sink tem que aguentar
// escrita concorrente sem corrida (validado com -race) e sem perder linha.
func TestFileLogSinkEscritaConcorrente(t *testing.T) {
	dir := t.TempDir()
	s := newFileLogSink(dir, 1<<20)
	if s == nil {
		t.Fatal("sink não abriu")
	}
	var wg sync.WaitGroup
	const linhas = 200
	for i := 0; i < linhas; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.write("linha concorrente\n")
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(filepath.Join(dir, logFileName))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "\n"); got != linhas {
		t.Fatalf("%d linhas gravadas, esperado %d", got, linhas)
	}
}

// Sink nulo (disco recusou) é silenciosamente inerte — o agente nunca depende do log em arquivo.
func TestFileLogSinkNuloEhInerte(t *testing.T) {
	var s *fileLogSink
	s.write("não explode\n")
}
