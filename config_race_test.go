package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// resetRuntimeConfig deixa o estado publicado num ponto conhecido. `runtimeState` é global por
// desenho (é o estado do processo), então cada teste que o usa precisa partir do zero.
func resetRuntimeConfig(t *testing.T, cfg *Config) {
	t.Helper()
	runtimeState.cfg.Store(nil)
	if cfg != nil {
		SetRuntimeConfig(cfg)
	}
}

func testConfig() *Config {
	return &Config{
		Server:        "https://api.exemplo.test",
		Name:          "Agente de Teste",
		TokenFile:     "/tmp/token-inexistente.txt",
		PrintSettings: "fit",
		PrinterTypes:  map[string]string{"inicial": "a4"},
	}
}

// O nome default deixou de ser o literal fixo "Agente" — dois
// devices sem nome configurado na mesma conta pareavam sob o mesmo `(account_id, name)` e
// canibalizavam o token um do outro. Comparado contra `os.Hostname()`
// direto, não contra um valor fixado no teste: a asserção fica presa à função real, não a uma
// suposição sobre qual é o hostname da máquina que roda o teste.
func TestDefaultDeviceNameUsaOHostnameDaMaquina(t *testing.T) {
	wantHost, err := os.Hostname()
	if err != nil || strings.TrimSpace(wantHost) == "" {
		t.Skip("os.Hostname() indisponível neste ambiente — nada para comparar")
	}
	if got := defaultDeviceName(); got != wantHost {
		t.Fatalf("defaultDeviceName() = %q, esperado o hostname da máquina %q", got, wantHost)
	}
	if got := defaultDeviceName(); got == "Agente" {
		t.Fatal("defaultDeviceName() voltou ao literal fixo \"Agente\" com hostname disponível")
	}
}

// Os dois pontos de entrada que decidiam o nome default (config nova, config existente com
// Name vazio) usam a mesma resolução — nenhum dos dois deveria fixar "Agente" enquanto
// os.Hostname() funcionar.
func TestNomeDefaultEHostnameNosDoisCaminhos(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		t.Skip("os.Hostname() indisponível neste ambiente — nada para comparar")
	}

	if got := defaultConfig().Name; got != host {
		t.Fatalf("defaultConfig().Name = %q, esperado hostname %q", got, host)
	}

	cfg := &Config{Name: "   "}
	normalizeConfig(cfg)
	if cfg.Name != host {
		t.Fatalf("normalizeConfig com Name vazio = %q, esperado hostname %q", cfg.Name, host)
	}
}

// A config publicada é uma **cópia**. Sem isto o chamador continuaria segurando uma
// referência viva para o estado compartilhado — o ponteiro que os dois tickers mutavam.
func TestSetRuntimeConfigPublicaCopiaNaoOPonteiroRecebido(t *testing.T) {
	original := testConfig()
	resetRuntimeConfig(t, original)

	publicada := GetRuntimeConfig()
	if publicada == original {
		t.Fatal("GetRuntimeConfig devolveu o mesmo ponteiro passado a Set: não houve cópia")
	}
	if publicada.PrinterTypes == nil {
		t.Fatal("mapa perdido na cópia")
	}

	// Mutar o original (o que o código de chamada fazia à vontade) não pode alcançar a publicada.
	original.PrinterTypes["inicial"] = "termica"
	original.Name = "outro"
	if got := publicada.PrinterTypes["inicial"]; got != "a4" {
		t.Fatalf("mapa da publicada mudou junto com o original: %q", got)
	}
	if publicada.Name != "Agente de Teste" {
		t.Fatalf("Name da publicada mudou junto com o original: %q", publicada.Name)
	}
}

// Clone precisa ser profundo no mapa — copiar só o struct deixaria o `concurrent map writes`
// alcançável com a aparência de resolvido.
func TestCloneEProfundoNoMapa(t *testing.T) {
	c := testConfig()
	cp := c.Clone()
	if cp == c {
		t.Fatal("Clone devolveu o mesmo ponteiro")
	}
	cp.PrinterTypes["novo"] = "laser"
	if _, ok := c.PrinterTypes["novo"]; ok {
		t.Fatal("mapa compartilhado entre original e clone")
	}
	if len(cp.PrinterTypes) != 2 || cp.PrinterTypes["inicial"] != "a4" {
		t.Fatalf("clone perdeu conteúdo: %v", cp.PrinterTypes)
	}

	semMapa := &Config{Name: "x"}
	if clone := semMapa.Clone(); clone.PrinterTypes != nil {
		t.Fatal("Clone inventou mapa onde não havia")
	}
	var nilCfg *Config
	if nilCfg.Clone() != nil {
		t.Fatal("Clone de nil deveria ser nil")
	}
}

// A prova central. Escritores de sincronização remota, escritores do painel e leitores
// concorrentes sobre a mesma config — o cenário exato do defeito original (dois tickers escrevendo em
// `cfg.PrinterTypes` enquanto o painel e as goroutines de job liam).
//
// Sob `-race` este teste mata: mutação do ponteiro publicado, Clone raso e ausência de
// sincronização. A asserção de contagem no fim mata um quarto defeito que o `-race` **não** pega:
// load-mutate-store sem CAS, que não é corrida de dados — é atualização perdida em silêncio.
func TestConfigSobSyncPainelELeituraConcorrentes(t *testing.T) {
	resetRuntimeConfig(t, testConfig())

	const escritoresSync = 40
	const escritoresPainel = 20
	const leitores = 40

	var wg sync.WaitGroup
	pronto := make(chan struct{})

	for i := 0; i < escritoresSync; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-pronto
			// Mesma forma de `syncDeviceConfig`: política remota entrando no mapa de tipos.
			policy := devicePolicy{printerTypes: map[string]string{fmt.Sprintf("impressora-%d", i): "a4"}}
			UpdateRuntimeConfig(func(c *Config) { applyDevicePolicy(c, policy) })
		}(i)
	}

	for i := 0; i < escritoresPainel; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-pronto
			nome := fmt.Sprintf("Sala %d", i)
			UpdateRuntimeConfig(func(c *Config) { applyEnrollInput(c, nome, "", "") })
		}(i)
	}

	for i := 0; i < leitores; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-pronto
			for j := 0; j < 50; j++ {
				cfg := GetRuntimeConfig()
				if cfg == nil {
					continue
				}
				// Leituras reais do agente: o painel lê Name/Server, o job lê o mapa de tipos.
				_ = cfg.Name
				_ = cfg.Server
				_, _ = lookupPrinterType(cfg, map[string]string{"id": "impressora-1", "name": "impressora-1"})
				for k, v := range cfg.PrinterTypes {
					_, _ = k, v
				}
			}
		}()
	}

	close(pronto)
	wg.Wait()

	final := GetRuntimeConfig()
	for i := 0; i < escritoresSync; i++ {
		key := fmt.Sprintf("impressora-%d", i)
		if _, ok := final.PrinterTypes[key]; !ok {
			t.Fatalf("atualização perdida: %q não está na config final (%d chaves) — cópia-e-troca sem CAS",
				key, len(final.PrinterTypes))
		}
	}
	if !strings.HasPrefix(final.Name, "Sala ") {
		t.Fatalf("Name final = %q, esperado uma das escritas do painel", final.Name)
	}
}

// Um snapshot já lido nunca muda debaixo de quem o segura. É o que permite a uma goroutine
// de job usar `cfg.PrinterTypes` sem lock enquanto a sync remota publica outra política.
func TestSnapshotNaoMudaDepoisDePublicado(t *testing.T) {
	resetRuntimeConfig(t, testConfig())

	snap := GetRuntimeConfig()
	tiposAntes := len(snap.PrinterTypes)
	nomeAntes := snap.Name

	for i := 0; i < 20; i++ {
		policy := devicePolicy{
			printSettings: "noscale",
			printerTypes:  map[string]string{fmt.Sprintf("nova-%d", i): "termica"},
		}
		UpdateRuntimeConfig(func(c *Config) { applyDevicePolicy(c, policy) })
		UpdateRuntimeConfig(func(c *Config) { applyEnrollInput(c, "Renomeado", "", "") })
	}

	if got := len(snap.PrinterTypes); got != tiposAntes {
		t.Fatalf("snapshot ganhou %d chaves depois de publicado", got-tiposAntes)
	}
	if snap.Name != nomeAntes {
		t.Fatalf("Name do snapshot mudou: %q → %q", nomeAntes, snap.Name)
	}
	if snap.PrintSettings != "fit" {
		t.Fatalf("PrintSettings do snapshot mudou: %q", snap.PrintSettings)
	}
	if novo := GetRuntimeConfig(); len(novo.PrinterTypes) != tiposAntes+20 {
		t.Fatalf("config publicada tem %d chaves, esperado %d", len(novo.PrinterTypes), tiposAntes+20)
	}
}

// `syncDeviceConfig` — o call site original do defeito — publica sem tocar em nada que já
// foi lido, mesmo quando duas chamadas correm junto com leitores.
func TestSyncDeviceConfigConcorrenteNaoMutaOQueJaFoiLido(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"printerTypes":{"ext-1":"a4","ext-2":"thermal"},"printSettings":"noscale"}`))
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.Server = srv.URL
	resetRuntimeConfig(t, cfg)

	snap := GetRuntimeConfig()
	tiposAntes := len(snap.PrinterTypes)

	var wg sync.WaitGroup
	pronto := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-pronto
			if err := syncDeviceConfig(context.Background(), "token-de-teste"); err != nil {
				t.Errorf("syncDeviceConfig: %v", err)
			}
		}()
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-pronto
			for j := 0; j < 100; j++ {
				c := GetRuntimeConfig()
				for k := range c.PrinterTypes {
					_ = k
				}
			}
		}()
	}
	close(pronto)
	wg.Wait()

	if got := len(snap.PrinterTypes); got != tiposAntes {
		t.Fatalf("snapshot lido antes da sync ganhou chaves: %d → %d", tiposAntes, got)
	}
	final := GetRuntimeConfig()
	if final.PrinterTypes["ext-1"] != "a4" || final.PrinterTypes["ext-2"] != "thermal" {
		t.Fatalf("política remota não foi aplicada: %v", final.PrinterTypes)
	}
	if final.PrintSettings != "noscale" {
		t.Fatalf("PrintSettings = %q, esperado noscale", final.PrintSettings)
	}
}

// Sem `localWritableKeys` — nunca existiu do lado do servidor
// (medido: `backend/api/src/routes/print-agent.ts` nunca populou o campo, nem antes desta feature),
// então "local vence" nunca foi alcançável em produção. O que sobra é a regra simples: servidor
// vence quando manda algo não vazio; política vazia nunca apaga o valor local (o servidor não tem
// como "limpar" um campo mandando ausência, só mandando um novo valor).
func TestApplyDevicePolicyServidorVenceQuandoNaoVazio(t *testing.T) {
	cases := []struct {
		name     string
		base     Config
		policy   devicePolicy
		wantPS   string
		wantSuma string
	}{
		{
			name:   "servidor vence com printSettings preenchido",
			base:   Config{PrintSettings: "fit"},
			policy: devicePolicy{printSettings: "noscale"},
			wantPS: "noscale",
		},
		{
			name:     "servidor vence com sumatraPdfPath preenchido",
			base:     Config{SumatraPdfPath: "C:/local.exe"},
			policy:   devicePolicy{sumatraPdfPath: "C:/remoto.exe"},
			wantSuma: "C:/remoto.exe",
		},
		{
			name:   "política vazia não apaga o local",
			base:   Config{PrintSettings: "fit"},
			policy: devicePolicy{printSettings: "   "},
			wantPS: "fit",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.base
			applyDevicePolicy(&c, tc.policy)
			if tc.wantPS != "" && c.PrintSettings != tc.wantPS {
				t.Fatalf("PrintSettings = %q, esperado %q", c.PrintSettings, tc.wantPS)
			}
			if tc.wantSuma != "" && c.SumatraPdfPath != tc.wantSuma {
				t.Fatalf("SumatraPdfPath = %q, esperado %q", c.SumatraPdfPath, tc.wantSuma)
			}
		})
	}
}

// As duas formas do corpo do device-config (aninhada em `policy` e plana) continuam aceitas — é o
// contrato 1.x que o agente promete não mexer. `etag`/`localWritableKeys` aparecem nos dois corpos de
// propósito: provam que um corpo antigo/hipotético que ainda os
// mande não quebra o decode — só não populam mais campo nenhum de `devicePolicy`, que os removeu.
func TestDecodeDevicePolicyAceitaAsDuasFormas(t *testing.T) {
	plana := `{"printerTypes":{"a":"a4"},"printSettings":"noscale","etag":"e1","localWritableKeys":["printSettings"]}`
	p, err := decodeDevicePolicy([]byte(plana))
	if err != nil {
		t.Fatalf("forma plana: %v", err)
	}
	if p.printSettings != "noscale" || p.printerTypes["a"] != "a4" {
		t.Fatalf("forma plana decodificada errado: %+v", p)
	}

	aninhada := `{"policy":{"printerTypes":{"b":"thermal"},"sumatraPdfPath":"C:/s.exe"},"etag":"e2"}`
	p, err = decodeDevicePolicy([]byte(aninhada))
	if err != nil {
		t.Fatalf("forma aninhada: %v", err)
	}
	if p.printerTypes["b"] != "thermal" || p.sumatraPdfPath != "C:/s.exe" {
		t.Fatalf("forma aninhada decodificada errado: %+v", p)
	}

	if _, err := decodeDevicePolicy([]byte(`nao é json`)); err == nil {
		t.Fatal("corpo inválido deveria devolver erro")
	}
}

// O ticker redundante de 30 min não existe mais. Ele não fazia trabalho novo — repetia o
// `syncDeviceConfig` do ciclo de 60 s — e era a segunda escritora que tornava a escrita no mapa
// *concorrente*. Este teste é a versão executável do "ticker de 30 min inexistente" da task.
func TestTickerRedundanteDe30MinNaoExisteMais(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("ler main.go: %v", err)
	}
	for lineNo, line := range stripComments(string(src)) {
		if strings.Contains(line, "30 * time.Minute") || strings.Contains(line, "30*time.Minute") {
			t.Fatalf("main.go:%d ressuscitou o ticker de 30 min: %s", lineNo+1, strings.TrimSpace(line))
		}
	}
}
