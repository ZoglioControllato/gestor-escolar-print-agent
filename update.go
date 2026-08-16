package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"print-agent/internal/api"
)

// ErrMissingSHA256 marca um manifest sem hash para o artefato (PPE-18, AD-011). O contrato do
// auto-update exige verificação **obrigatória**: um binário que roda como SYSTEM/root instalado sem
// checagem de integridade é a superfície de ataque mais direta que este agente tem — bastaria
// comprometer a origem do manifest (mesmo domínio do app web) para trocar o binário de toda a frota.
var ErrMissingSHA256 = errors.New("update: manifest sem sha256 para este artefato — recusado (AD-011)")

type versionArtifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type agentVersionManifest struct {
	Version   string                     `json:"version"`
	Artifacts map[string]versionArtifact `json:"artifacts"`
}

// agentArtifactKey identifica o artefato no print-agent-version.json (386 Windows, arm64 Linux).
func agentArtifactKey() string {
	if runtime.GOOS == "windows" {
		return "386"
	}
	return runtime.GOARCH
}

// versionCheckBaseURL origem do app web (mesma estratégia do frontend /version.json).
func versionCheckBaseURL(cfg *Config) string {
	if app := strings.TrimSpace(cfg.AppURL); app != "" {
		return strings.TrimRight(app, "/")
	}
	server := strings.TrimRight(cfg.Server, "/")
	if idx := strings.Index(server, "://api."); idx >= 0 {
		return server[:idx+3] + "app." + server[idx+len("://api."):]
	}
	return server
}

func resolveArtifactURL(base, artifactURL string) string {
	artifactURL = strings.TrimSpace(artifactURL)
	if artifactURL == "" {
		return ""
	}
	if strings.HasPrefix(artifactURL, "http://") || strings.HasPrefix(artifactURL, "https://") {
		return artifactURL
	}
	if strings.HasPrefix(artifactURL, "/") {
		return strings.TrimRight(base, "/") + artifactURL
	}
	return strings.TrimRight(base, "/") + "/" + artifactURL
}

// checkAndApplyUpdate consulta print-agent-version.json no app web e aplica update se houver.
// checkAndApplyUpdate verifica, baixa e aplica uma versão nova, e reinicia o processo. `inFlight`
// (`runner.InFlight`, PR-11 da feature 038) é a trava que impede o restart de atropelar uma rajada de
// impressão: `Print()` não é cancelável (`printFile`, `cmd.CombinedOutput()` sem `CommandContext`), e
// desde que o claim passou a acontecer para o lote inteiro antes de imprimir qualquer um (PR-11),
// vários jobs podem estar reivindicados ao mesmo tempo — reiniciar no meio deles reproduziria o PPE-30
// (job preso em `printing` para sempre) para vários jobs de uma vez, não só um. Checado duas vezes:
// antes de baixar (evita trabalho à toa) e de novo antes de `restartSelf` (a trava que importa de
// verdade — a impressão pode ter começado durante o download). Se ocupado, este ciclo é pulado; o
// próximo `ticker` (6h) tenta de novo — não há retry mais cedo, é a mesma cadência de sempre.
func checkAndApplyUpdate(ctx context.Context, cfg *Config, inFlight func() int) {
	base := versionCheckBaseURL(cfg)
	url := fmt.Sprintf("%s/print-agent-version.json?t=%d", base, time.Now().UnixMilli())

	resp, err := apiClient.Do(ctx, api.Request{
		Method: http.MethodGet,
		URL:    url,
		Header: map[string]string{"Cache-Control": "no-cache", "Pragma": "no-cache"},
	})
	if err != nil {
		logf("[UPDATE] Erro ao verificar versão: %v", err)
		return
	}

	if resp.StatusCode != http.StatusOK {
		logf("[UPDATE] Servidor retornou %d ao verificar versão", resp.StatusCode)
		return
	}

	var manifest agentVersionManifest
	if err := json.Unmarshal(resp.Body, &manifest); err != nil {
		logf("[UPDATE] Erro ao parsear print-agent-version.json: %v", err)
		return
	}

	remoteVersion := strings.TrimSpace(manifest.Version)
	if remoteVersion == "" || remoteVersion == agentVersion {
		logf("[UPDATE] Versão atual %s é a mais recente", agentVersion)
		return
	}

	key := agentArtifactKey()
	artifact, ok := manifest.Artifacts[key]
	if !ok || strings.TrimSpace(artifact.URL) == "" {
		logf("[UPDATE] Nova versão %s disponível, mas sem artefato para %s", remoteVersion, key)
		return
	}

	logf("[UPDATE] Nova versão disponível: %s (atual: %s)", remoteVersion, agentVersion)

	if n := inFlight(); n > 0 {
		logf("[UPDATE] Atualização adiada: %d job(s) de impressão em voo. Tenta de novo no próximo ciclo (6h).", n)
		return
	}

	if err := downloadAndApply(ctx, cfg, artifact); err != nil {
		logf("[UPDATE] Falha ao aplicar update: %v", err)
		return
	}

	if n := inFlight(); n > 0 {
		// Binário já trocado em disco; o processo atual (ainda a versão antiga em memória) segue
		// rodando os jobs em voo. O próximo ciclo (6h) repete o download (idempotente — mesmo
		// manifest) e, achando o processo livre, reinicia.
		logf("[UPDATE] Binário atualizado, mas reinício adiado: %d job(s) de impressão em voo. Reinicia no próximo ciclo (6h).", n)
		return
	}

	logf("[UPDATE] Update aplicado com sucesso! Reiniciando...")
	restartSelf()
}

// resolveExecutablePath retorna o caminho absoluto do binário corrente (resolvendo symlinks).
func resolveExecutablePath() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("não foi possível obter caminho do executável: %w", err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return "", fmt.Errorf("symlink eval: %w", err)
	}
	return exePath, nil
}

// downloadToTemp baixa artifact.URL para um arquivo temporário e valida SHA-256 (se presente).
// Retorna caminho do arquivo baixado. Chamador é responsável por remover.
func downloadToTemp(ctx context.Context, cfg *Config, artifact versionArtifact, tmpSuffix string) (string, error) {
	downloadURL := resolveArtifactURL(versionCheckBaseURL(cfg), artifact.URL)
	if downloadURL == "" {
		return "", fmt.Errorf("URL do artefato vazia")
	}

	resp, err := apiClient.Open(ctx, api.Request{Method: http.MethodGet, URL: downloadURL})
	if err != nil {
		return "", fmt.Errorf("erro ao baixar update: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download retornou HTTP %d", resp.StatusCode)
	}

	tmpFile, err := os.CreateTemp("", "gestor-escolar-update-*"+tmpSuffix)
	if err != nil {
		return "", fmt.Errorf("erro ao criar arquivo temporário: %w", err)
	}
	tmpPath := tmpFile.Name()

	hasher := sha256.New()
	writer := io.MultiWriter(tmpFile, hasher)

	n, err := io.Copy(writer, resp.Body)
	tmpFile.Close()
	if err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("erro ao salvar update: %w", err)
	}

	logf("[UPDATE] Baixados %d bytes", n)

	// PPE-18: SHA-256 é obrigatório, não condicional. Um manifest sem hash não vira "instala sem
	// checar" — vira recusa explícita. `os.Remove(tmpPath)` aqui é o que impede o download órfão de
	// sobreviver à recusa.
	if strings.TrimSpace(artifact.SHA256) == "" {
		os.Remove(tmpPath)
		return "", ErrMissingSHA256
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(got, artifact.SHA256) {
		os.Remove(tmpPath)
		return "", fmt.Errorf("SHA-256 inválido: esperado %s, obtido %s", artifact.SHA256, got)
	}
	logf("[UPDATE] SHA-256 verificado ✓")

	return tmpPath, nil
}

// extractAgentBinaryFromTarGz extrai entry "*/gestor-escolar" do tar.gz e escreve em destPath.
// Retorna erro se nenhuma entry de binário for encontrada.
func extractAgentBinaryFromTarGz(tarGzPath, destPath string) error {
	src, err := os.Open(tarGzPath)
	if err != nil {
		return fmt.Errorf("abrir tarball: %w", err)
	}
	defer src.Close()

	gz, err := gzip.NewReader(src)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if path.Base(hdr.Name) != "gestor-escolar" {
			continue
		}

		out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
		if err != nil {
			return fmt.Errorf("criar destino: %w", err)
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			os.Remove(destPath)
			return fmt.Errorf("extrair binário: %w", err)
		}
		out.Close()
		return nil
	}

	return fmt.Errorf("binário gestor-escolar não encontrado no tarball")
}

// swapBinary substitui exePath pelo conteúdo de newBinPath de forma atômica.
// Move atual para .old; em caso de falha no rename final, restaura .old.
func swapBinary(exePath, newBinPath string) error {
	oldPath := exePath + ".old"
	_ = os.Remove(oldPath)

	if err := os.Rename(exePath, oldPath); err != nil {
		return fmt.Errorf("backup do executável (verifique permissão de escrita em %s): %w", filepath.Dir(exePath), err)
	}

	if err := os.Rename(newBinPath, exePath); err != nil {
		_ = os.Rename(oldPath, exePath)
		return fmt.Errorf("instalar novo executável: %w", err)
	}

	_ = os.Chmod(exePath, 0755)
	return nil
}

func downloadAndApply(ctx context.Context, cfg *Config, artifact versionArtifact) error {
	exePath, err := resolveExecutablePath()
	if err != nil {
		return err
	}

	switch runtime.GOOS {
	case "windows":
		return applyWindowsUpdate(ctx, cfg, artifact, exePath)
	case "linux":
		return applyLinuxUpdate(ctx, cfg, artifact, exePath)
	default:
		return fmt.Errorf("auto-update não suportado em GOOS=%s", runtime.GOOS)
	}
}

// applyWindowsUpdate baixa .exe direto e substitui o binário.
func applyWindowsUpdate(ctx context.Context, cfg *Config, artifact versionArtifact, exePath string) error {
	tmpPath, err := downloadToTemp(ctx, cfg, artifact, ".exe")
	if err != nil {
		return err
	}
	if err := swapBinary(exePath, tmpPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// applyLinuxUpdate baixa .tar.gz, extrai binário "gestor-escolar" e substitui o atual.
func applyLinuxUpdate(ctx context.Context, cfg *Config, artifact versionArtifact, exePath string) error {
	tarPath, err := downloadToTemp(ctx, cfg, artifact, ".tar.gz")
	if err != nil {
		return err
	}
	defer os.Remove(tarPath)

	newBinPath := exePath + ".new"
	if err := extractAgentBinaryFromTarGz(tarPath, newBinPath); err != nil {
		_ = os.Remove(newBinPath)
		return err
	}

	if err := swapBinary(exePath, newBinPath); err != nil {
		_ = os.Remove(newBinPath)
		return err
	}
	return nil
}

// removeStaleUpdateBackup apaga o `.old` deixado por um swap anterior (PPE-18).
//
// `.old` só deveria existir entre o instante do swap e o próximo startup — antes desta task ele
// nunca era removido (achado #5 do diagnóstico), então cada auto-update aplicado (a cada 6h, AD-011)
// deixava mais um binário morto no disco, para sempre. `exePath` vem injetado para o teste não
// depender de `os.Executable()`.
func removeStaleUpdateBackup(exePath string) {
	if strings.TrimSpace(exePath) == "" {
		return
	}
	oldPath := exePath + ".old"
	if _, err := os.Stat(oldPath); err != nil {
		return
	}
	// Limpa o atributo somente-leitura antes de remover: no Windows, os.Remove falha em arquivo
	// read-only com "Acesso negado" (mesmo motivo de clearPairToken em config.go).
	_ = os.Chmod(oldPath, 0666)
	if err := os.Remove(oldPath); err != nil {
		logf("[UPDATE] Aviso: não foi possível remover backup obsoleto %s: %v", oldPath, err)
		return
	}
	logf("[UPDATE] Backup obsoleto removido: %s", oldPath)
}

// scheduleUpdateChecks verifica print-agent-version.json a cada 6 horas (primeira após 2 min).
// `inFlight` (`runner.InFlight`) é repassado a `checkAndApplyUpdate` — ver o comentário lá (PR-11).
func scheduleUpdateChecks(ctx context.Context, cfg *Config, inFlight func() int) {
	// PPE-18: o `.old` do swap anterior (se houver) é varrido no startup seguinte, antes de qualquer
	// outra coisa do ciclo de update.
	if exePath, err := resolveExecutablePath(); err == nil {
		removeStaleUpdateBackup(exePath)
	}

	time.AfterFunc(2*time.Minute, func() {
		checkAndApplyUpdate(ctx, cfg, inFlight)
	})

	go func() {
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				checkAndApplyUpdate(ctx, cfg, inFlight)
			}
		}
	}()
}
