package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Padrões proibidos em arquivo de produção do agente (PPE-25).
//
// Cada um foi um defeito medido no diagnóstico da feature (§3 achado #4):
//   - `http.DefaultClient`, `http.Get`, `http.Post` — cliente **sem timeout algum**: uma conexão
//     pendurada congelava o agente até reinício manual.
//   - `req, _ :=` — erro de `http.NewRequest` descartado, seguido de dereferência do nil.
//   - `http.NewRequest(` sem `WithContext` — requisição sem cancelamento.
//
// A varredura existe porque o critério da task é "zero ocorrências", e um critério de ausência não
// é verificável sem um lugar que o verifique: um `grep` rodado à mão uma vez não impede a
// reintrodução na próxima PR. Esta é a versão executável do mesmo grep.
var forbiddenHTTPPatterns = []struct {
	pattern string
	why     string
}{
	{"http.DefaultClient", "cliente global sem timeout — use internal/api"},
	{"http.Get(", "sem timeout e sem context — use internal/api"},
	{"http.Post(", "sem timeout e sem context — use internal/api"},
	{"http.PostForm(", "sem timeout e sem context — use internal/api"},
	{"http.Head(", "sem timeout e sem context — use internal/api"},
	{"req, _ :=", "erro de construção de requisição descartado — nil deref"},
	{"areq, _ :=", "erro de construção de requisição descartado — nil deref"},
	{"http.NewRequest(", "sem context — use http.NewRequestWithContext"},
}

// goProductionFiles devolve todo .go do agente que não é teste, incluindo os de outras plataformas
// (`//go:build windows`): a varredura lê texto, então o arquivo do Windows é coberto rodando no
// Linux — que é exatamente onde este gate roda.
func goProductionFiles(t *testing.T) []string {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	var files []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "dist", "releases", "assets", "installer", "agent", ".git":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(files) < 10 {
		t.Fatalf("varredura encontrou só %d arquivos .go — o caminho está errado", len(files))
	}
	return files
}

// stripComments devolve as linhas do fonte com comentário removido, preservando a numeração.
//
// É o mesmo recorte que a varredura de idioma do pgrcc-obras faz: a regra é sobre **código**, e o
// comentário é justamente onde o padrão proibido precisa poder ser citado — este arquivo e o
// cabeçalho de `internal/api/client.go` citam `http.DefaultClient` e `req, _ :=` ao explicar por que
// são proibidos, e uma varredura que os pegasse tornaria a explicação impossível de escrever.
func stripComments(src string) []string {
	lines := strings.Split(src, "\n")
	inBlock := false
	for i, line := range lines {
		var out strings.Builder
		for j := 0; j < len(line); j++ {
			if inBlock {
				if strings.HasPrefix(line[j:], "*/") {
					inBlock = false
					j++
				}
				continue
			}
			if strings.HasPrefix(line[j:], "//") {
				break
			}
			if strings.HasPrefix(line[j:], "/*") {
				inBlock = true
				j++
				continue
			}
			out.WriteByte(line[j])
		}
		lines[i] = out.String()
	}
	return lines
}

// PPE-25: nenhum arquivo de produção fala HTTP sem timeout, sem context ou descartando o erro de
// construção da requisição.
func TestNenhumCallSiteHTTPSemTimeoutOuContext(t *testing.T) {
	for _, file := range goProductionFiles(t) {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("ler %s: %v", file, err)
		}
		// O próprio pacote api é onde o `*http.Client` nasce; ele não pode usar os padrões
		// proibidos, e não usa — por isso não há exceção nenhuma nesta lista.
		for lineNo, line := range stripComments(string(data)) {
			for _, forbidden := range forbiddenHTTPPatterns {
				if strings.Contains(line, forbidden.pattern) {
					t.Errorf("%s:%d usa %q (%s):\n\t%s",
						file, lineNo+1, forbidden.pattern, forbidden.why, strings.TrimSpace(line))
				}
			}
		}
	}
}

// O stripper precisa apagar comentário e **só** comentário: um teste que apagasse código demais
// deixaria a varredura passar em silêncio, que é o modo de falha caro aqui.
func TestStripCommentsPreservaCodigoENumeracao(t *testing.T) {
	src := "a := 1 // http.Get( no comentário\n" +
		"/* bloco\n" +
		"http.DefaultClient aqui dentro\n" +
		"*/ b := http.Get(x)\n" +
		"c := 3\n"
	got := stripComments(src)
	if len(got) != 6 {
		t.Fatalf("linhas = %d, esperado 6 (numeração preservada)", len(got))
	}
	if !strings.Contains(got[0], "a := 1") || strings.Contains(got[0], "http.Get(") {
		t.Fatalf("linha 1 = %q", got[0])
	}
	if strings.Contains(got[2], "http.DefaultClient") {
		t.Fatalf("linha 3 (dentro de bloco) não foi apagada: %q", got[2])
	}
	if !strings.Contains(got[3], "http.Get(x)") {
		t.Fatalf("linha 4 perdeu o código depois do fecha-bloco: %q", got[3])
	}
	if !strings.Contains(got[4], "c := 3") {
		t.Fatalf("linha 5 = %q", got[4])
	}
}
