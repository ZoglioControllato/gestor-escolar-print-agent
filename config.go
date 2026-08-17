package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const defaultProductionServer = "https://api.pedagogicoonline.com.br"

// fixedGUIPort é a porta HTTP local do painel (Windows).
const fixedGUIPort = 17345

type Config struct {
	Server         string            `json:"server"`
	AppURL         string            `json:"appUrl"`
	Name           string            `json:"name"`
	TokenFile      string            `json:"tokenFile"`
	SumatraPdfPath string            `json:"sumatraPdfPath"`
	PrintSettings  string            `json:"printSettings"`
	PrinterTypes   map[string]string `json:"printerTypes,omitempty"`
	EnrollmentKey  string            `json:"enrollmentKey"`

	// Events é o bloco `events` do `GET /print-agent/device-config`.
	//
	// `json:"-"` de propósito: é política do servidor, renovada a cada 60 s, e não pertence ao
	// `config.json` do disco — persistir um endpoint faria o kill-switch precisar de um segundo
	// caminho de limpeza. Nulo significa **opere em poll**, que é o estado da frota 1.x, o do
	// kill-switch ligado e o de um ambiente onde a stack Amplify ainda não publicou nada.
	Events *EventsConfig `json:"-"`
}

// EventsConfig espelha o bloco `events` do device-config. Os endpoints são **hosts**, não
// URLs: quem monta `wss://<host>/event/realtime` é o cliente.
type EventsConfig struct {
	Enabled          bool   `json:"enabled"`
	RealtimeEndpoint string `json:"realtimeEndpoint"`
	HTTPHost         string `json:"httpHost"`
	Channel          string `json:"channel"`
}

// Equal compara duas configurações de push. O supervisor do transporte usa isto para decidir se
// precisa derrubar e reabrir a conexão: reabrir a cada sync de 60 s desperdiçaria o handshake e
// ressincronizaria a frota inteira num mesmo minuto.
func (e *EventsConfig) Equal(other *EventsConfig) bool {
	if e == nil || other == nil {
		return e == other
	}
	return *e == *other
}

// Clone devolve uma cópia profunda: o mapa `PrinterTypes` é recriado, não compartilhado.
//
// É o que torna a config publicada imutável na prática. Copiar só o struct deixaria o mapa
// apontando para o mesmo objeto — o `fatal error: concurrent map writes` continuaria alcançável, com
// a aparência de estar resolvido.
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	cp := *c
	if c.PrinterTypes != nil {
		cp.PrinterTypes = make(map[string]string, len(c.PrinterTypes))
		for k, v := range c.PrinterTypes {
			cp.PrinterTypes[k] = v
		}
	}
	return &cp
}

func configBaseDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// configDataDirOverride, quando não-vazio, substitui o caminho padrão do SO — hook só de teste
// (nunca setado em produção). Sem ele, testes que gravam config.json/token.txt tentam criar
// /var/lib/gestor-escolar de verdade, que exige root e falha em runners CI sem privilégio.
var configDataDirOverride string

func configDataDir() string {
	dir := configDataDirOverride
	if dir == "" {
		switch runtime.GOOS {
		case "windows":
			dir = configBaseDir()
		default:
			dir = "/var/lib/gestor-escolar"
		}
	}
	_ = os.MkdirAll(dir, 0755)
	return dir
}

func configFilePath() string {
	return filepath.Join(configDataDir(), "config.json")
}

func normalizeServerURL(server string) string {
	s := strings.TrimSpace(server)
	s = strings.TrimRight(s, "/")
	if s == "" {
		return defaultProductionServer
	}
	return s
}

func defaultAppURL(server string) string {
	s := normalizeServerURL(server)
	if strings.Contains(s, "api-devel.") {
		return "https://app-devel.pedagogicoonline.com.br"
	}
	if strings.Contains(s, "api.") {
		return strings.Replace(s, "api.", "app.", 1)
	}
	return "https://app.pedagogicoonline.com.br"
}

// defaultDeviceName resolve o nome default do device para o **hostname** da máquina — não
// mais o literal fixo `"Agente"`. O pareamento casa por `(account_id, name)`
// (`backend/api/src/routes/print-agent.ts`): dois agentes sem nome configurado na mesma conta
// canibalizavam o token um do outro, porque os dois pareavam sob o mesmo nome. `os.Hostname()`
// falha só em ambientes bem restritos (achado prático raro); nesse caso cai de volta no literal
// antigo, para nunca devolver string vazia.
func defaultDeviceName() string {
	if h, err := os.Hostname(); err == nil {
		if h = strings.TrimSpace(h); h != "" {
			return h
		}
	}
	return "Agente"
}

func defaultConfig() *Config {
	dataDir := configDataDir()
	return &Config{
		Server:         defaultProductionServer,
		AppURL:         defaultAppURL(defaultProductionServer),
		Name:           defaultDeviceName(),
		TokenFile:      filepath.Join(dataDir, "token.txt"),
		SumatraPdfPath: "SumatraPDF.exe",
		PrintSettings:  "fit",
	}
}

func normalizeConfig(cfg *Config) {
	cfg.Server = normalizeServerURL(cfg.Server)
	if strings.TrimSpace(cfg.AppURL) == "" {
		cfg.AppURL = defaultAppURL(cfg.Server)
	}
	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = defaultDeviceName()
	}
	dataDir := configDataDir()
	if strings.TrimSpace(cfg.TokenFile) == "" {
		cfg.TokenFile = filepath.Join(dataDir, "token.txt")
	} else if !filepath.IsAbs(cfg.TokenFile) {
		cfg.TokenFile = filepath.Join(dataDir, filepath.Base(cfg.TokenFile))
	}
	if strings.TrimSpace(cfg.SumatraPdfPath) == "" {
		cfg.SumatraPdfPath = "SumatraPDF.exe"
	} else if !filepath.IsAbs(cfg.SumatraPdfPath) {
		exePath := filepath.Join(configBaseDir(), cfg.SumatraPdfPath)
		if _, err := os.Stat(exePath); err == nil {
			cfg.SumatraPdfPath = exePath
		}
	}
}

func loadConfig() (*Config, error) {
	path := configFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := defaultConfig()
			normalizeConfig(cfg)
			return cfg, nil
		}
		return nil, fmt.Errorf("ler config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config inválido: %w", err)
	}
	normalizeConfig(&cfg)
	return &cfg, nil
}

func saveConfig(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config nil")
	}
	normalizeConfig(cfg)
	dir := configDataDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("criar pasta de dados: %w", err)
	}
	cfg.TokenFile = filepath.Join(dir, "token.txt")

	persist := struct {
		Server         string `json:"server"`
		AppURL         string `json:"appUrl,omitempty"`
		Name           string `json:"name"`
		TokenFile      string `json:"tokenFile"`
		SumatraPdfPath string `json:"sumatraPdfPath,omitempty"`
		PrintSettings  string `json:"printSettings,omitempty"`
		EnrollmentKey  string `json:"enrollmentKey,omitempty"`
	}{
		Server:         cfg.Server,
		AppURL:         cfg.AppURL,
		Name:           cfg.Name,
		TokenFile:      "token.txt",
		SumatraPdfPath: filepath.Base(cfg.SumatraPdfPath),
		PrintSettings:  cfg.PrintSettings,
		EnrollmentKey:  strings.TrimSpace(cfg.EnrollmentKey),
	}
	if persist.SumatraPdfPath == "" || persist.SumatraPdfPath == "." {
		persist.SumatraPdfPath = "SumatraPDF.exe"
	}

	data, err := json.MarshalIndent(persist, "", "  ")
	if err != nil {
		return fmt.Errorf("serializar config: %w", err)
	}
	path := filepath.Join(dir, "config.json")
	// 0600: config.json guarda a enrollmentKey — qualquer
	// usuário local com 0644 conseguia ler o segredo que autoriza pareamento de novos devices na
	// conta. No Windows, o bit Unix não tem efeito nenhum sozinho: restrictFilePermissions aplica a
	// ACL de verdade (best-effort, logado — nunca falha a gravação por causa disso).
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("gravar config: %w", err)
	}
	if err := restrictFilePermissions(path); err != nil {
		logf("[SEC] Aviso: não foi possível restringir ACL de %s: %v", path, err)
	}
	cfg.TokenFile = filepath.Join(dir, "token.txt")
	return nil
}

// tempDir returns the directory used for temporary print job PDFs.
func tempDir() string {
	dir := filepath.Join(configDataDir(), "temp")
	_ = os.MkdirAll(dir, 0755)
	return dir
}

// sweepOrphanTempFiles remove arquivos de `dir` cuja última modificação é mais velha que `maxAge`,
// relativo a `now`.
//
// Substitui a goroutine dedicada de 3 min por job: antes, cada job
// agendava sua própria `time.Sleep` + remoção — um crash ou `kill -9` no meio da espera deixava o
// PDF órfão no disco para sempre, e nada varria o diretório no próximo start. Aqui é **um** laço
// para o processo inteiro (chamado tanto no startup, com `maxAge` zero — nada deveria estar lá se o
// agente não estava rodando —, quanto periodicamente por um reaper único, com `maxAge` de 3 min: a
// mesma janela de graça de antes para o spooler Windows reacessar o arquivo, sem gastar uma
// goroutine por impressão). `now` é injetado para o teste não depender de `time.Sleep` real.
func sweepOrphanTempFiles(dir string, maxAge time.Duration, now time.Time) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < maxAge {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
			removed++
		}
	}
	return removed
}

// ensureDataFiles creates the data directory structure on first run:
//   - config.json  (writes defaults if missing)
//   - token.txt    (creates empty file if missing, so the path is writable)
//   - temp/        (directory for transient print-job PDFs)
//
// Returns the first error encountered, but continues creating the remaining
// items so callers can log a single meaningful failure.
func ensureDataFiles(cfg *Config) error {
	var firstErr error

	// temp/ directory
	if err := os.MkdirAll(tempDir(), 0755); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("criar pasta temp: %w", err)
	}

	// config.json — write defaults only when the file does not yet exist
	cfgPath := filepath.Join(configDataDir(), "config.json")
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		// Clone porque saveConfig normaliza (e portanto muta) o que recebe: passar a config
		// publicada aqui a mutaria pelas costas de todo mundo que já a leu.
		if serr := saveConfig(cfg.Clone()); serr != nil && firstErr == nil {
			firstErr = fmt.Errorf("criar config.json: %w", serr)
		}
	}

	// token.txt — create empty placeholder so the path is always present. 0600: é o
	// segredo de autenticação do device, e 0644 deixava qualquer usuário local lê-lo.
	if _, err := os.Stat(cfg.TokenFile); os.IsNotExist(err) {
		if ferr := os.WriteFile(cfg.TokenFile, []byte{}, 0600); ferr != nil && firstErr == nil {
			firstErr = fmt.Errorf("criar token.txt: %w", ferr)
		} else if ferr == nil {
			if rerr := restrictFilePermissions(cfg.TokenFile); rerr != nil {
				logf("[SEC] Aviso: não foi possível restringir ACL de %s: %v", cfg.TokenFile, rerr)
			}
		}
	}

	return firstErr
}

// applyEnrollInput escreve os campos que o painel edita, ignorando os vazios (o formulário manda o
// que o usuário deixou em branco). Vive fora do handler porque UpdateRuntimeConfig pode reexecutá-la
// no laço de CAS, e porque é ela que o candidato validado e a config publicada compartilham — duas
// grafias da mesma regra divergiriam.
func applyEnrollInput(c *Config, name, server, enrollmentKey string) {
	if name != "" {
		c.Name = name
	}
	if server != "" {
		c.Server = normalizeServerURL(server)
		c.AppURL = defaultAppURL(c.Server)
	}
	if enrollmentKey != "" {
		c.EnrollmentKey = enrollmentKey
	}
}

func clearPairToken(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config nil")
	}
	// Clone: normalizeConfig muta, e cfg pode ser a config publicada.
	local := cfg.Clone()
	normalizeConfig(local)
	// Clear read-only attribute before removing; on Windows, os.Remove fails
	// for read-only files with "Acesso negado" / "Access is denied".
	_ = os.Chmod(local.TokenFile, 0666)
	if err := os.Remove(local.TokenFile); err != nil && !os.IsNotExist(err) {
		return err
	}
	SetTrayToken("")
	return nil
}

func rePair(ctx context.Context, cfg *Config) (string, error) {
	if err := clearPairToken(cfg); err != nil {
		return "", err
	}
	return pair(ctx, cfg)
}
