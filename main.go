package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"print-agent/internal/api"
	"print-agent/internal/events"
	"print-agent/internal/jobs"
)

// apiClient é a **única** porta HTTP do agente (PPE-25). Um cliente por processo: reaproveita
// conexão, e — o que importa — nenhum caminho fala com a rede sem deadline.
var apiClient = api.New()

// serverURL monta uma URL do backend a partir da rota do agente.
func serverURL(cfg *Config, path string) string {
	return normalizeServerURL(cfg.Server) + printAgentAPIPrefix + path
}

// agentVersion é substituível em build: go build -ldflags "-X main.agentVersion=1.2.3"
var agentVersion = "1.0.0"

// isRunningAsService é true quando o processo foi iniciado pelo SCM do Windows.
// Usado por restartSelf() para escolher a estratégia correta de reinício.
var isRunningAsService bool

const printAgentAPIPrefix = "/print-agent"

// errAgentUnauthorized marca uma resposta 401 do servidor REST (PPE-20) — a mesma reação do lado
// WS (events.Client.Unauthorized()): não adianta insistir, o token é que está recusado.
var errAgentUnauthorized = errors.New("agent: servidor recusou o token (401)")

// sleepCtx espera `d`, mas devolve mais cedo se `ctx` for cancelado — usado em todo laço/backoff do
// agente que precisa parar de verdade num SIGTERM/stop de serviço (PPE-30), em vez de um
// `time.Sleep` surdo ao cancelamento.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func logf(msg string, args ...interface{}) {
	t := time.Now().Format("2006-01-02 15:04:05")
	line := fmt.Sprintf("[%s] %s\n", t, fmt.Sprintf(msg, args...))
	fmt.Print(line)
	// Espelho em disco (agent.log, ver logfile.go) — best-effort, nil-safe.
	logSink.write(line)
}

func pair(ctx context.Context, cfg *Config) (string, error) {
	payload := map[string]string{
		"name":         cfg.Name,
		"agentVersion": agentVersion,
		"platform":     runtime.GOOS + "/" + runtime.GOARCH,
	}
	if strings.TrimSpace(cfg.EnrollmentKey) != "" {
		payload["enrollmentKey"] = strings.TrimSpace(cfg.EnrollmentKey)
	}
	body, _ := json.Marshal(payload)
	resp, err := apiClient.Do(ctx, api.Request{
		Method:      http.MethodPost,
		URL:         serverURL(cfg, "/pair"),
		ContentType: "application/json",
		Body:        body,
	})
	if err != nil {
		return "", fmt.Errorf("erro ao conectar: %w", err)
	}

	bodyBytes := resp.Body

	if resp.StatusCode >= 400 {
		var eb struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(bodyBytes, &eb)
		if eb.Error != "" {
			return "", fmt.Errorf("%s", eb.Error)
		}
		return "", fmt.Errorf("pair falhou (HTTP %d)", resp.StatusCode)
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return "", fmt.Errorf("resposta inválida: %w", err)
	}
	if result.Token == "" {
		return "", fmt.Errorf("token não retornado")
	}

	// 0600 (PPE-22): token.txt é a credencial do device — 0644 deixava qualquer usuário local lê-la
	// e se passar pelo agente perante o servidor.
	if err := os.WriteFile(cfg.TokenFile, []byte(result.Token), 0600); err != nil {
		return "", fmt.Errorf("erro ao salvar token: %w", err)
	}
	if err := restrictFilePermissions(cfg.TokenFile); err != nil {
		logf("[SEC] Aviso: não foi possível restringir ACL de %s: %v", cfg.TokenFile, err)
	}
	SetTrayToken(result.Token)
	return result.Token, nil
}

// devicePolicy é a política remota já decodificada, separada da aplicação para que a decisão de
// precedência (servidor × local) seja testável sem rede.
type devicePolicy struct {
	printSettings  string
	sumatraPdfPath string
	printerTypes   map[string]string

	// events é o bloco de push. Nulo = "opere em poll" — bloco ausente (frota 1.x, kill-switch
	// ligado, ambiente sem a stack Amplify) e `enabled:false` são a mesma instrução (PPE-28).
	events *EventsConfig
}

// decodeDevicePolicy lê o corpo do device-config nas duas formas que o agente aceita desde sempre:
// aninhada em `policy` e plana na raiz. Não decodifica mais `etag`/`localWritableKeys` (PPE-33,
// 034-print-push-events/T30): o servidor nunca enviou nenhum dos dois campos — nem hoje, nem antes
// desta feature (medido em `backend/api/src/routes/print-agent.ts`) — então o mecanismo de
// cache/`localWritableKeys` nunca executou em produção; era código morto desde sempre, não uma
// regressão desta task.
func decodeDevicePolicy(body []byte) (devicePolicy, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return devicePolicy{}, fmt.Errorf("device-config JSON: %w", err)
	}

	var p devicePolicy

	if policyRaw, ok := raw["policy"]; ok {
		var policy struct {
			PrintSettings  string            `json:"printSettings"`
			SumatraPdfPath string            `json:"sumatraPdfPath"`
			PrinterTypes   map[string]string `json:"printerTypes"`
		}
		if err := json.Unmarshal(policyRaw, &policy); err == nil {
			p.printSettings = policy.PrintSettings
			p.sumatraPdfPath = policy.SumatraPdfPath
			p.printerTypes = policy.PrinterTypes
		}
	} else {
		if psRaw, ok := raw["printSettings"]; ok {
			_ = json.Unmarshal(psRaw, &p.printSettings)
		}
		if spRaw, ok := raw["sumatraPdfPath"]; ok {
			_ = json.Unmarshal(spRaw, &p.sumatraPdfPath)
		}
		if ptRaw, ok := raw["printerTypes"]; ok {
			_ = json.Unmarshal(ptRaw, &p.printerTypes)
		}
	}
	if eventsRaw, ok := raw["events"]; ok {
		var ev EventsConfig
		if json.Unmarshal(eventsRaw, &ev) == nil && ev.Enabled {
			p.events = &ev
		}
	}

	return p, nil
}

// applyDevicePolicy escreve a política sobre `c`, que é sempre a **cópia** entregue por
// UpdateRuntimeConfig — nunca a config publicada (PPE-29). É pura sobre `c`: pode ser reexecutada
// quando o CAS falha.
//
// Servidor vence quando manda um valor não vazio — sem gate de `localWritableKeys` (PPE-33,
// 034-print-push-events/T30: removido junto com o etag, nunca populado de verdade). O bug real que
// isso corrigiu não era o gate em si — era o backend mandar `printSettings: "noscale"`
// **incondicionalmente**, hardcoded, sobrescrevendo o valor local a cada sync de 60 s; o servidor
// parou de mandar esse literal (`backend/api/src/routes/print-agent.ts`), então esta função só
// aplica `printSettings` quando o servidor de fato tiver um valor real para enviar.
func applyDevicePolicy(c *Config, p devicePolicy) {
	if ps := strings.TrimSpace(p.printSettings); ps != "" {
		c.PrintSettings = ps
	}

	if sp := strings.TrimSpace(p.sumatraPdfPath); sp != "" {
		c.SumatraPdfPath = sp
	}

	for k, v := range p.printerTypes {
		if strings.TrimSpace(k) == "" {
			continue
		}
		if c.PrinterTypes == nil {
			c.PrinterTypes = map[string]string{}
		}
		c.PrinterTypes[k] = v
	}

	// Atribuição direta, **não** merge: o bloco ausente precisa apagar o que havia. É essa linha
	// que faz o kill-switch (`PRINT_EVENTS_ENABLED=false`) devolver a frota ao poll em ≤60 s sem
	// redeploy de agente (PPE-10) — um merge deixaria o endpoint antigo vivo para sempre.
	c.Events = p.events
}

// syncDeviceConfig aplica política remota (GET /print-agent/device-config): servidor é fonte de
// verdade para todo campo que mandar preenchido.
//
// Sem cache de ETag/`If-None-Match` nem ack (PPE-33, 034-print-push-events/T30 — removido dos dois
// lados): o servidor nunca implementou a metade que faria o cache valer a pena — nunca manda
// `ETag`/304, nunca leu o corpo do POST /device-config/ack que recebia (só devolvia `{ok:true}`) —
// então era round-trip a mais por sync sem nenhum ganho, não uma otimização real.
//
// Não recebe `*Config` de propósito: a versão anterior recebia o ponteiro compartilhado e escrevia
// direto no mapa dele, e era esse o caminho do `fatal error: concurrent map writes` do achado #2.
// Aqui a política vai para uma cópia, publicada atomicamente.
func syncDeviceConfig(ctx context.Context, token string) error {
	cfg := GetRuntimeConfig()
	if cfg == nil {
		return fmt.Errorf("config não publicada")
	}

	resp, err := apiClient.Do(ctx, api.Request{
		Method: http.MethodGet,
		URL:    serverURL(cfg, "/device-config"),
		Token:  token,
	})
	if err != nil {
		return err
	}

	if resp.Unauthorized() {
		return errAgentUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("device-config HTTP %d", resp.StatusCode)
	}

	policy, err := decodeDevicePolicy(resp.Body)
	if err != nil {
		return err
	}

	UpdateRuntimeConfig(func(c *Config) { applyDevicePolicy(c, policy) })
	return nil
}

func mapPrinterStatusWindows(printerStatus int, workOffline bool, detectedErrorState int, jobCount int) (status string, statusMsg string) {
	// Prioridade: WorkOffline > DetectedErrorState > PrinterStatus
	if workOffline {
		return "offline", "Impressora em modo offline (Use Printer Offline)"
	}
	if detectedErrorState == 9 {
		return "offline", "Offline"
	}
	if detectedErrorState != 0 && detectedErrorState != 2 {
		switch detectedErrorState {
		case 3:
			return "error", "Pouco papel"
		case 4:
			return "error", "Sem papel"
		case 5:
			return "error", "Pouco toner"
		case 6:
			return "error", "Sem toner"
		case 7:
			return "error", "Porta aberta"
		case 8:
			return "error", "Papel preso"
		case 10:
			return "error", "Manutenção necessária"
		case 11, 12:
			return "error", "Problema com papel"
		default:
			return "error", fmt.Sprintf("Erro %d", detectedErrorState)
		}
	}
	if jobCount > 0 {
		return "printing", ""
	}
	switch printerStatus {
	case 3:
		return "ready", ""
	case 4:
		return "printing", ""
	case 7:
		return "offline", "Offline"
	case 1, 2:
		return "unknown", ""
	case 5:
		return "printing", "Aquecendo"
	case 6:
		return "paused", "Parada"
	default:
		return "unknown", ""
	}
}

func getPrinters() ([]map[string]string, error) {
	if runtime.GOOS == "windows" {
		return getPrintersWindows()
	}
	return getPrintersLinux()
}

func getPrintersWindows() ([]map[string]string, error) {
	script := `Get-CimInstance -Class Win32_Printer | Select-Object Name, PrinterStatus, WorkOffline, DetectedErrorState | ConvertTo-Json -Compress`
	cmd := hiddenCommand("powershell", "-NoProfile", "-Command", script)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("erro ao listar impressoras: %w", err)
	}

	var printers []map[string]string
	var items []struct {
		Name               string `json:"Name"`
		PrinterStatus      int    `json:"PrinterStatus"`
		WorkOffline        bool   `json:"WorkOffline"`
		DetectedErrorState int    `json:"DetectedErrorState"`
	}

	data := strings.TrimSpace(string(out))
	if data == "" {
		return printers, nil
	}

	if strings.HasPrefix(data, "[") {
		if err := json.Unmarshal([]byte(data), &items); err != nil {
			return nil, fmt.Errorf("erro ao parsear JSON: %w", err)
		}
	} else {
		var single struct {
			Name               string `json:"Name"`
			PrinterStatus      int    `json:"PrinterStatus"`
			WorkOffline        bool   `json:"WorkOffline"`
			DetectedErrorState int    `json:"DetectedErrorState"`
		}
		if err := json.Unmarshal([]byte(data), &single); err != nil {
			return nil, fmt.Errorf("erro ao parsear JSON: %w", err)
		}
		items = []struct {
			Name               string `json:"Name"`
			PrinterStatus      int    `json:"PrinterStatus"`
			WorkOffline        bool   `json:"WorkOffline"`
			DetectedErrorState int    `json:"DetectedErrorState"`
		}{single}
	}

	for _, p := range items {
		if p.Name == "" {
			continue
		}
		status, statusMsg := mapPrinterStatusWindows(p.PrinterStatus, p.WorkOffline, p.DetectedErrorState, 0)
		printers = append(printers, map[string]string{
			"id":            p.Name,
			"name":          p.Name,
			"status":        status,
			"statusMessage": statusMsg,
		})
	}
	return printers, nil
}

func getPrintersLinux() ([]map[string]string, error) {
	cmd := exec.Command("lpstat", "-p")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("erro ao listar impressoras: %w", err)
	}

	var printers []map[string]string
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "printer ") {
			continue
		}
		line = strings.TrimPrefix(line, "printer ")
		parts := strings.SplitN(line, " ", 2)
		if len(parts) < 2 {
			continue
		}
		name := parts[0]
		rest := strings.ToLower(parts[1])
		status := "unknown"
		statusMsg := ""
		switch {
		case strings.Contains(rest, "is idle"):
			status = "ready"
		case strings.Contains(rest, "now printing"):
			status = "printing"
		case strings.Contains(rest, "disabled"):
			status = "offline"
			if idx := strings.Index(rest, "disabled"); idx >= 0 && idx+8 < len(rest) {
				statusMsg = strings.TrimSpace(rest[idx+8:])
			}
		case strings.Contains(rest, "not ready") || strings.Contains(rest, "error"):
			status = "error"
			statusMsg = rest
		}
		printers = append(printers, map[string]string{
			"id":            name,
			"name":          name,
			"status":        status,
			"statusMessage": statusMsg,
		})
	}
	return printers, nil
}

var printerTypeLabels = map[string]string{
	"thermal": "Impressora Térmica de Recibos",
	"a4":      "Impressora Papel A4",
	"laser":   "Impressora Laser",
	"inkjet":  "Impressora Jato de Tinta",
	"label":   "Impressora de Etiquetas",
	"matrix":  "Impressora Matricial",
}

func resolvePrinterType(alias string) string {
	if label, ok := printerTypeLabels[strings.ToLower(strings.TrimSpace(alias))]; ok {
		return label
	}
	return alias // se não for um alias conhecido, usa o valor literal
}

func lookupPrinterType(cfg *Config, printer map[string]string) (string, bool) {
	if len(cfg.PrinterTypes) == 0 {
		return "", false
	}
	if alias, ok := cfg.PrinterTypes[printer["id"]]; ok {
		return alias, true
	}
	if alias, ok := cfg.PrinterTypes[printer["name"]]; ok {
		return alias, true
	}
	return "", false
}

func patchDeviceName(ctx context.Context, cfg *Config, token, name string) error {
	body, _ := json.Marshal(map[string]string{"name": strings.TrimSpace(name)})
	resp, err := apiClient.Do(ctx, api.Request{
		Method:      http.MethodPatch,
		URL:         serverURL(cfg, "/device"),
		Token:       token,
		ContentType: "application/json",
		Body:        body,
	})
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("rename remoto HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(resp.Body)))
	}
	return nil
}

func sendPrinters(ctx context.Context, cfg *Config, token string) error {
	printers, err := getPrinters()
	if err != nil {
		return err
	}
	if len(printers) == 0 {
		logf("Nenhuma impressora encontrada")
		return nil
	}

	// Tipos vindos do servidor via syncDeviceConfig (admin web é fonte de verdade)
	if len(cfg.PrinterTypes) > 0 {
		for _, p := range printers {
			if alias, ok := lookupPrinterType(cfg, p); ok {
				p["printerType"] = resolvePrinterType(alias)
			}
		}
	}

	body, _ := json.Marshal(printers)
	resp, err := apiClient.Do(ctx, api.Request{
		Method:      http.MethodPost,
		URL:         serverURL(cfg, "/printers"),
		Token:       token,
		ContentType: "application/json",
		Body:        body,
	})
	if err != nil {
		return fmt.Errorf("erro ao enviar impressoras: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("servidor retornou %d", resp.StatusCode)
	}
	logf("Impressoras enviadas: %d", len(printers))
	return nil
}

func downloadPDFFromUrl(ctx context.Context, downloadUrl, jobId string) (string, error) {
	logf("[DEBUG] downloadPDFFromUrl: GET %s", downloadUrl)
	resp, err := apiClient.Open(ctx, api.Request{Method: http.MethodGet, URL: downloadUrl})
	if err != nil {
		return "", fmt.Errorf("erro ao baixar: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logf("[DEBUG] downloadPDF: resposta %d", resp.StatusCode)
		return "", fmt.Errorf("download retornou %d", resp.StatusCode)
	}

	path := filepath.Join(tempDir(), jobId+".pdf")
	f, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("erro ao criar arquivo: %w", err)
	}
	defer f.Close()

	n, err := io.Copy(f, resp.Body)
	if err != nil {
		os.Remove(path)
		return "", fmt.Errorf("erro ao salvar: %w", err)
	}
	logf("[DEBUG] downloadPDF: %d bytes salvos em %s", n, path)
	return path, nil
}

// reportStatus informa o servidor sobre o andamento de um job.
//
// Devolve erro — inclusive para resposta não-2xx — porque quem chama precisa distinguir "o servidor
// registrou" de "não registrou": é essa distinção que impede a reimpressão infinita do achado #3 do
// diagnóstico, onde o report era fire-and-forget e um `completed` perdido deixava o job `queued`
// para sempre.
func reportStatus(ctx context.Context, cfg *Config, token, jobId, status, errorMsg string) error {
	body := map[string]string{"jobId": jobId, "status": status}
	if errorMsg != "" {
		body["errorMessage"] = errorMsg
	}
	jsonBody, _ := json.Marshal(body)

	logf("[DEBUG] reportStatus: jobId=%s status=%s errorMsg=%q", jobId, status, errorMsg)
	resp, err := apiClient.Do(ctx, api.Request{
		Method:      http.MethodPost,
		URL:         serverURL(cfg, "/job-status"),
		Token:       token,
		ContentType: "application/json",
		Body:        jsonBody,
	})
	if err != nil {
		logf("Erro ao reportar status: %v", err)
		return err
	}
	logf("[DEBUG] reportStatus: resposta HTTP %d body=%s", resp.StatusCode, string(resp.Body))
	if resp.Unauthorized() {
		return errAgentUnauthorized
	}
	if !resp.OK() {
		return fmt.Errorf("job-status HTTP %d", resp.StatusCode)
	}
	return nil
}

func sumatraExists(path string) bool {
	if path == "" {
		logf("[DEBUG] sumatraExists: path vazio")
		return false
	}
	if _, err := os.Stat(path); err == nil {
		logf("[DEBUG] sumatraExists: encontrado em %s (os.Stat)", path)
		return true
	}
	if _, err := exec.LookPath(path); err == nil {
		logf("[DEBUG] sumatraExists: encontrado via PATH (LookPath)")
		return true
	}
	logf("[DEBUG] sumatraExists: não encontrado em %s", path)
	return false
}

func printFile(cfg *Config, printerName, filePath string) error {
	if runtime.GOOS == "windows" {
		sumatraPath := cfg.SumatraPdfPath
		if sumatraPath == "" {
			sumatraPath = "SumatraPDF.exe"
		}
		// Resolver para caminho absoluto antes de exec.Command (Go 1.19+ bloqueia caminhos relativos no Windows)
		if absSumatra, err := filepath.Abs(sumatraPath); err == nil {
			sumatraPath = absSumatra
		}
		if sumatraExists(sumatraPath) {
			logf("[INFO] SumatraPDF encontrado: %s", sumatraPath)
			absFile, _ := filepath.Abs(filePath)
			args := []string{"-silent", "-print-to", printerName}
			if cfg.PrintSettings != "" {
				args = append(args, "-print-settings", cfg.PrintSettings)
				logf("[DEBUG] print-settings: %q", cfg.PrintSettings)
			} else {
				logf("[DEBUG] print-settings: não definido (usando padrão do SumatraPDF)")
			}
			args = append(args, "-exit-on-print", absFile)
			logf("[DEBUG] comando: %s %s", sumatraPath, strings.Join(args, " "))
			cmd := hiddenCommand(sumatraPath, args...)
			out, err := cmd.CombinedOutput()
			if len(out) > 0 {
				logf("[DEBUG] SumatraPDF output: %s", string(out))
			}
			outTrim := strings.TrimSpace(string(out))
			if err != nil {
				if outTrim != "" {
					return fmt.Errorf("%w | output: %s", err, outTrim)
				}
				return err
			}
			if outTrim != "" {
				logf("[DEBUG] SumatraPDF saiu com código 0 mas gerou output: %s", outTrim)
			}
			return nil
		}
		logf("[WARN] SumatraPDF NÃO encontrado em %s — usando fallback lp/powershell (pode causar erro 'arquivo não existe' no spooler)", sumatraPath)
		cmd := hiddenCommand("lp", "-d", printerName, filePath)
		outLp, errLp := cmd.CombinedOutput()
		if errLp == nil {
			logf("[DEBUG] Impressão via lp concluída")
			return nil
		}
		outLpTrim := strings.TrimSpace(string(outLp))
		if outLpTrim != "" {
			logf("[DEBUG] lp output: %s", outLpTrim)
		}
		logf("[DEBUG] lp falhou, tentando powershell Start-Process -Verb Print")
		cmd = hiddenCommand("powershell", "-NoProfile", "-Command",
			fmt.Sprintf("Start-Process -FilePath %q -Verb Print -Wait", filePath))
		outPs, errPs := cmd.CombinedOutput()
		if len(outPs) > 0 {
			logf("[DEBUG] PowerShell output: %s", string(outPs))
		}
		if errPs != nil {
			outPsTrim := strings.TrimSpace(string(outPs))
			if outPsTrim != "" {
				return fmt.Errorf("powershell print failed: %w | output: %s", errPs, outPsTrim)
			}
			if outLpTrim != "" {
				return fmt.Errorf("powershell print failed: %w | lp output: %s", errPs, outLpTrim)
			}
			return fmt.Errorf("powershell print failed: %w", errPs)
		}
		return nil
	}
	cmd := exec.Command("lp", "-d", printerName, filePath)
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		logf("[DEBUG] lp output: %s", string(out))
	}
	if err != nil {
		outTrim := strings.TrimSpace(string(out))
		if outTrim != "" {
			return fmt.Errorf("%w | output: %s", err, outTrim)
		}
		return err
	}
	return nil
}

// downloadAndPrint baixa e imprime um job. É a metade do antigo `processJob` que **não** fala de
// estado: reportar `printing`/`completed`/`failed` e decidir se o job já está em voo passou a ser
// responsabilidade do `jobs.Runner`, que é quem garante a unicidade (PPE-09).
func downloadAndPrint(ctx context.Context, job jobs.Job) error {
	cfg := GetRuntimeConfig()
	if cfg == nil {
		return fmt.Errorf("config não publicada")
	}
	logf("Processando job %s para impressora %s", job.ID, job.PrinterExternalID)

	t1 := time.Now()
	path, err := downloadPDFFromUrl(ctx, job.DownloadURL, job.ID)
	logf("[TIMING] job=%s downloadPDFFromUrl levou %v", job.ID, time.Since(t1).Round(time.Millisecond))
	if err != nil {
		return fmt.Errorf("[download] %w", err)
	}
	// A remoção do PDF temporário não é mais uma goroutine dedicada por job (PPE-31,
	// diagnostic.md §3 achado #7: uma `time.Sleep(3 * time.Minute)` + goroutine para cada job era
	// uma goroutine a mais por impressão, e um crash/kill antes dos 3 min deixava o arquivo órfão
	// para sempre — nada varria o diretório temp no próximo start). O laço único de
	// `sweepOrphanTempFiles` em `run()` cobre os dois casos: a mesma janela de graça de 3 min antes
	// de apagar (o spooler pode reacessar o arquivo pouco depois do print retornar) e a varredura de
	// órfãos no startup.

	t2 := time.Now()
	err = printFile(cfg, job.PrinterExternalID, path)
	logf("[TIMING] job=%s printFile levou %v", job.ID, time.Since(t2).Round(time.Millisecond))
	if err != nil {
		return fmt.Errorf("[print:%s] %w", job.PrinterExternalID, err)
	}
	logf("Job %s concluído", job.ID)
	return nil
}

// pendingJob é um item da fila devolvida por `GET /print-agent/pending-jobs`.
type pendingJob struct {
	JobId             string `json:"jobId"`
	Id                string `json:"id"`
	PrinterExternalId string `json:"printerExternalId"`
	DownloadUrl       string `json:"downloadUrl"`
}

// ID resolve o identificador do job: o servidor devolve `id` e `jobId` com o mesmo valor, e o
// agente aceita os dois desde sempre.
func (j pendingJob) ID() string {
	if j.Id != "" {
		return j.Id
	}
	return j.JobId
}

// fetchPendingJobs busca a fila do device. Devolve também o cooldown pedido pelo servidor num 429
// (`Retry-After`/`pollIntervalMs`, PPE-06) — quem o honra é o consumidor.
func fetchPendingJobs(ctx context.Context, cfg *Config, token string) (jobs []pendingJob, cooldown time.Duration, err error) {
	url := serverURL(cfg, "/pending-jobs")
	logf("[DEBUG] fetchPendingJobs: GET %s", url)
	resp, err := apiClient.Do(ctx, api.Request{Method: http.MethodGet, URL: url, Token: token})
	if err != nil {
		return nil, 0, err
	}
	if resp.RateLimited() {
		return nil, resp.Cooldown(), nil
	}
	if resp.Unauthorized() {
		return nil, 0, errAgentUnauthorized
	}
	if !resp.OK() {
		return nil, resp.Cooldown(), fmt.Errorf("pending-jobs HTTP %d", resp.StatusCode)
	}
	var result struct {
		Jobs []pendingJob `json:"jobs"`
	}
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return nil, 0, fmt.Errorf("pending-jobs JSON: %w", err)
	}
	valid := make([]pendingJob, 0, len(result.Jobs))
	for _, j := range result.Jobs {
		if j.ID() != "" && j.PrinterExternalId != "" && j.DownloadUrl != "" {
			valid = append(valid, j)
		}
	}
	return valid, 0, nil
}

// toRunnerJobs converte a fila do servidor para o formato do consumidor.
func toRunnerJobs(pending []pendingJob) []jobs.Job {
	out := make([]jobs.Job, 0, len(pending))
	for _, j := range pending {
		out = append(out, jobs.Job{
			ID:                j.ID(),
			PrinterExternalID: j.PrinterExternalId,
			DownloadURL:       j.DownloadUrl,
		})
	}
	return out
}

// Intervalos do laço de consumo (design.md § Fluxos por estado do transporte).
const (
	// pollFallbackInterval é o intervalo com o WebSocket fora. 10 s é a latência efetiva de hoje —
	// o rate limit do servidor já era de 10 s —, então uma queda de push **não** regride a UX.
	pollFallbackInterval = 10 * time.Second

	// pollReconcileInterval é a rede de segurança com o WebSocket saudável: cobre evento perdido e
	// publish que falhou (PPE-26). Não é o transporte; o transporte é o evento.
	pollReconcileInterval = 300 * time.Second

	// heartbeatInterval é a batida de presença (PPE-07). O servidor tolera 150 s (2,5 batidas), o
	// que absorve uma batida perdida sem marcar a escola como offline.
	heartbeatInterval = 60 * time.Second

	// Jitter: largo no fallback (onde a frota inteira está no mesmo ritmo depois de uma queda),
	// estreito na reconciliação e no heartbeat.
	pollFallbackJitter  = 0.2
	pollReconcileJitter = 0.1
	heartbeatJitter     = 0.1

	// tempFileMaxAge é a janela de graça antes de apagar um PDF de job já processado: o mesmo tempo
	// que a goroutine por job dava ao spooler Windows para reacessar o arquivo, antes de PPE-31
	// trocar por um reaper único (design.md § temp).
	tempFileMaxAge = 3 * time.Minute

	// tempSweepInterval é o ritmo do reaper periódico — não precisa ser fino, só menor que
	// tempFileMaxAge para não deixar arquivos velhos se acumulando por muito tempo entre varreduras.
	tempSweepInterval = 3 * time.Minute
)

// pollBaseInterval escolhe o ritmo do poll pelo estado do transporte.
//
// Só `subscribed` relaxa: `connecting` ainda não garante entrega de evento nenhum, e tratá-lo como
// saudável abriria uma janela de até 300 s em que nada chega — nem push, nem poll.
func pollBaseInterval(status events.Status) (time.Duration, float64) {
	if status == events.StatusSubscribed {
		return pollReconcileInterval, pollReconcileJitter
	}
	return pollFallbackInterval, pollFallbackJitter
}

// withJitter espalha um intervalo em ±frac. `rnd` devolve [0,1) e é injetado para o teste medir os
// extremos em vez de torcer pela distribuição.
func withJitter(d time.Duration, frac float64, rnd func() float64) time.Duration {
	if frac <= 0 || d <= 0 {
		return d
	}
	if rnd == nil {
		rnd = rand.Float64
	}
	delta := float64(d) * frac * (2*rnd() - 1)
	out := time.Duration(float64(d) + delta)
	if out < 0 {
		return 0
	}
	return out
}

// nextPollDelay é o intervalo até a próxima batida do poll, já com jitter.
func nextPollDelay(status events.Status, rnd func() float64) time.Duration {
	base, frac := pollBaseInterval(status)
	return withJitter(base, frac, rnd)
}

// transportLabel é o que o heartbeat reporta em `transport` — a telemetria que identifica as
// escolas cujo firewall bloqueia WSS (PPE-06/PPE-07). É um rótulo, nunca uma condição.
func transportLabel(status events.Status) string {
	if status == events.StatusSubscribed {
		return "ws"
	}
	return "poll"
}

// signalWake acorda o consumidor **sem bloquear**.
//
// O canal tem capacidade 1: um sinal pendente absorve os seguintes. É daí que sai o coalescing da
// spec — N eventos de uma rajada, mais o ticker que bate no meio dela, produzem **um** fetch, e o
// fetch devolve a fila inteira.
func signalWake(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// sendHeartbeat anuncia presença sem consultar a fila (PPE-07).
//
// O agente 1.x se anunciava de graça, a cada poll de 1 s. O agente vNext fica calado esperando o
// evento, então precisa de um sinal próprio — senão um device saudável com WebSocket assinado
// (poll de 300 s) estouraria o limite de 150 s do servidor e a escola receberia 409 ao mandar
// imprimir.
func sendHeartbeat(ctx context.Context, cfg *Config, token, transport string) error {
	body, _ := json.Marshal(map[string]string{"transport": transport, "agentVersion": agentVersion})
	resp, err := apiClient.Do(ctx, api.Request{
		Method:      http.MethodPost,
		URL:         serverURL(cfg, "/heartbeat"),
		Token:       token,
		ContentType: "application/json",
		Body:        body,
	})
	if err != nil {
		return err
	}
	if resp.Unauthorized() {
		return errAgentUnauthorized
	}
	if !resp.OK() {
		return fmt.Errorf("heartbeat HTTP %d", resp.StatusCode)
	}
	return nil
}

// eventsClientConfig monta a configuração do transporte a partir da config publicada. Devolve
// `false` quando não há push — bloco ausente, kill-switch ligado ou token ainda não pareado.
func eventsClientConfig(cfg *Config, token string) (events.Config, bool) {
	if cfg == nil || cfg.Events == nil || !cfg.Events.Enabled {
		return events.Config{}, false
	}
	ec := events.Config{
		RealtimeEndpoint: cfg.Events.RealtimeEndpoint,
		HTTPHost:         cfg.Events.HTTPHost,
		Channel:          cfg.Events.Channel,
		Token:            token,
	}
	return ec, ec.Valid()
}

// transport supervisiona o cliente WebSocket, mantendo-o alinhado com o device-config.
//
// Existe porque a configuração de push **muda em runtime**: o kill-switch pode desligá-la, um
// re-pareamento troca o token e a stack pode publicar o endpoint depois de o agente já estar no ar.
// Sem supervisor, cada uma dessas mudanças exigiria reiniciar o agente.
type transport struct {
	mu      sync.Mutex
	client  *events.Client
	cancel  context.CancelFunc
	current events.Config
}

// status é o estado do transporte, ou `disconnected` quando não há cliente.
func (t *transport) status() events.Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client == nil {
		return events.StatusDisconnected
	}
	return t.client.Status()
}

// apply liga, desliga ou reinicia o transporte conforme a configuração desejada. Configuração
// idêntica é no-op — reabrir a conexão a cada sync de 60 s desperdiçaria o handshake e
// ressincronizaria a frota inteira no mesmo minuto.
func (t *transport) apply(ctx context.Context, desired events.Config, enabled bool, wake chan struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !enabled {
		t.stopLocked()
		return
	}
	if t.client != nil && t.current == desired {
		return
	}
	t.stopLocked()

	client := events.New(desired,
		events.WithHTTPClient(apiClient.HTTPClient()),
		events.WithLogf(logf),
	)
	sessionCtx, cancel := context.WithCancel(ctx)
	t.client, t.cancel, t.current = client, cancel, desired

	go client.Run(sessionCtx)
	go func() {
		for {
			select {
			case <-sessionCtx.Done():
				return
			case <-client.Wake():
				signalWake(wake)
			case <-client.Unauthorized():
				// PPE-20: o cliente já parou sozinho (Client.Run devolve sem backoff numa
				// negativa de autorização — nenhum "insistir" do lado WS). O que falta é decidir
				// o que o agente inteiro faz — mesma reação de um 401 REST, por isso o mesmo
				// handleUnauthorized.
				logf("[EVENTS] transporte recusado pelo servidor")
				go handleUnauthorized(ctx, t, wake)
				return
			}
		}
	}()
	logf("[EVENTS] transporte ligado em %s", desired.Channel)
}

func (t *transport) stopLocked() {
	if t.cancel != nil {
		t.cancel()
	}
	t.client, t.cancel, t.current = nil, nil, events.Config{}
}

// unauthorizedSleep é a espera antes da 1ª (e única) tentativa de re-pair — var para o teste não
// dormir de verdade. Absorve rajadas de 401 quase simultâneas (heartbeat e poll caindo juntos): o
// gate de beginUnauthorizedEpisode já garante 1 reação só, esta espera é folga extra antes de gastar
// essa única tentativa.
var unauthorizedSleep = sleepCtx

// unauthorizedBackoff é o tempo de unauthorizedSleep.
const unauthorizedBackoff = 5 * time.Second

// handleUnauthorized reage à primeira negativa de autorização de um episódio (REST 401 ou WS,
// PPE-20): desliga o transporte push e, com `enrollmentKey` salva, tenta **exatamente uma** vez
// re-parear; sem chave salva, ou se essa tentativa falhar, marca o agente "Desvinculado" — nunca um
// loop de retentativas.
//
// Chamada de goroutines diferentes (poll, heartbeat, sync, o supervisor do WS) que podem disparar
// quase juntas; beginUnauthorizedEpisode garante que só a primeira faz alguma coisa.
func handleUnauthorized(ctx context.Context, tr *transport, wake chan struct{}) {
	if !beginUnauthorizedEpisode() {
		return
	}
	defer endUnauthorizedEpisode()

	logf("[AUTH] servidor recusou o token — parando transportes")
	tr.apply(ctx, events.Config{}, false, wake)

	cfg := GetRuntimeConfig()
	key := ""
	if cfg != nil {
		key = strings.TrimSpace(cfg.EnrollmentKey)
	}
	if key == "" {
		logf("[AUTH] sem enrollmentKey salva — estado Desvinculado")
		SetAgentUnauthorized(true)
		return
	}

	if err := unauthorizedSleep(ctx, unauthorizedBackoff); err != nil {
		// Contexto cancelado (shutdown) no meio da espera: nem tenta re-parear, mas o estado
		// Desvinculado também não faz sentido — o processo está saindo mesmo.
		return
	}

	logf("[AUTH] tentando re-pair (1 tentativa)")
	newToken, err := rePair(ctx, cfg)
	if err != nil {
		logf("[AUTH] re-pair falhou: %v — estado Desvinculado", err)
		SetAgentUnauthorized(true)
		return
	}

	logf("[AUTH] re-pair concluído — retomando")
	SetTrayToken(newToken)
	SetLastPairError("")
	SetAgentUnauthorized(false)
}

func run(ctx context.Context, cfg *Config) error {
	SetAgentName(cfg.Name)
	ensureGUIServer(cfg)

	// Ensure data directory structure exists on first run.
	// A failure here is non-fatal but logged so the user can diagnose
	// permission problems immediately instead of later during pair/print.
	if err := ensureDataFiles(cfg); err != nil {
		logf("Aviso: erro ao preparar diretório de dados: %v", err)
	}

	// PPE-31: varre PDFs órfãos do temp no startup. `maxAge` zero varre tudo — o agente não estava
	// rodando, então nada ali é "recente" de verdade (um job em voo de verdade não sobreviveria a um
	// restart do processo de qualquer forma, dado que o registro em voo é só em memória).
	if n := sweepOrphanTempFiles(tempDir(), 0, time.Now()); n > 0 {
		logf("[TEMP] %d arquivo(s) órfão(s) removido(s) no startup", n)
	}

	// Read token — treat empty file the same as missing (not yet paired).
	tokenBytes, _ := os.ReadFile(cfg.TokenFile)
	tokenStr := strings.TrimSpace(string(tokenBytes))

	if tokenStr == "" {
		logf("Token ausente ou vazio, iniciando pareamento...")
		for {
			// Token may have been set via the GUI panel (/api/enroll) while we sleep.
			if t := GetAgentToken(); t != "" {
				tokenStr = t
				SetLastPairError("")
				logf("Token recebido via painel local.")
				break
			}
			// A config pode ter mudado pelo painel (`/api/enroll` troca servidor e chave) —
			// relê a publicada a cada tentativa, em vez de insistir com a que veio do disco.
			if rc := GetRuntimeConfig(); rc != nil {
				cfg = rc
			}
			paired, pairErr := pair(ctx, cfg)
			if pairErr != nil {
				SetLastPairError(pairErr.Error())
				logf("Falha ao parear: %v. Tentando novamente em 60s...", pairErr)
				// PPE-30: sleepCtx (não time.Sleep) — um SIGTERM/stop do serviço durante o
				// pareamento não pode esperar até 60s para ser notado (achado (i) da fase
				// anterior). ctx cancelado aqui devolve o erro de cancelamento, que run()
				// propaga para fora do laço de pareamento em vez de tentar de novo.
				if err := sleepCtx(ctx, 60*time.Second); err != nil {
					logf("Cancelado durante pareamento.")
					return err
				}
				continue
			}
			SetLastPairError("")
			tokenStr = paired
			logf("Registrado e vinculado à conta. Device token: %s", tokenStr)
			break
		}
	}
	SetTrayToken(tokenStr)
	SetRuntimeConfig(cfg)

	if err := syncDeviceConfig(ctx, tokenStr); err != nil {
		logf("Aviso: sincronização de config remota: %v", err)
	}

	if err := sendPrinters(ctx, GetRuntimeConfig(), tokenStr); err != nil {
		logf("Aviso: %v", err)
	}

	// --- Consumidor único ---------------------------------------------------------------------
	//
	// Um canal `wake` de capacidade 1 e um consumidor. Todos os produtores (evento do WebSocket,
	// ticker de fallback/reconciliação, `subscribe_success`) empurram para o mesmo canal, e o
	// consumidor faz **um** fetch por ciclo — que devolve a fila inteira. Era aqui que ficava o
	// ticker de 1 segundo responsável pelas 86.400 requisições/dia/device.
	wake := make(chan struct{}, 1)
	tr := &transport{}
	runner := jobs.New(jobs.Deps{
		Fetch: func(ctx context.Context) ([]jobs.Job, time.Duration, error) {
			// PPE-20: "Desvinculado" não bate mais no servidor — é o que impede o loop quente
			// enquanto o agente espera uma ação (re-pair automático em andamento, ou manual via
			// painel).
			if IsAgentUnauthorized() {
				return nil, 0, nil
			}
			token := GetAgentToken()
			if token == "" {
				return nil, 0, nil
			}
			pending, cooldown, err := fetchPendingJobs(ctx, GetRuntimeConfig(), token)
			if errors.Is(err, errAgentUnauthorized) {
				go handleUnauthorized(ctx, tr, wake)
				return nil, 0, nil
			}
			return toRunnerJobs(pending), cooldown, err
		},
		Report: func(ctx context.Context, jobID, status, errMsg string) error {
			err := reportStatus(ctx, GetRuntimeConfig(), GetAgentToken(), jobID, status, errMsg)
			if errors.Is(err, errAgentUnauthorized) {
				go handleUnauthorized(ctx, tr, wake)
			}
			return err
		},
		Print: downloadAndPrint,
		Logf:  logf,
	})
	go runner.Run(ctx, wake)

	// Depois do runner existir (PR-11): o auto-update precisa de `runner.InFlight` para não
	// reiniciar no meio de uma rajada de impressão (ver comentário de `checkAndApplyUpdate`).
	scheduleUpdateChecks(ctx, GetRuntimeConfig(), runner.InFlight)

	// Drenagem inicial: o agente pode ter ficado fora do ar com jobs enfileirados.
	signalWake(wake)

	// --- Produtor: ticker de fallback / reconciliação ------------------------------------------
	//
	// 10 s com o transporte fora (= a latência efetiva de hoje, sem regressão de UX) e 300 s com
	// ele assinado (rede de segurança para evento perdido). O intervalo é recalculado a cada volta
	// porque o estado do transporte muda embaixo do laço.
	go func() {
		for {
			delay := nextPollDelay(tr.status(), nil)
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			signalWake(wake)
		}
	}()

	// --- Reaper do temp -----------------------------------------------------------------------
	//
	// PPE-31: uma varredura periódica para o processo inteiro, não uma goroutine por job. Roda no
	// mesmo ritmo da janela de graça (`tempFileMaxAge`), então um PDF não sobrevive muito além do
	// tempo que o spooler Windows precisa para reacessá-lo.
	go func() {
		ticker := time.NewTicker(tempSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if n := sweepOrphanTempFiles(tempDir(), tempFileMaxAge, time.Now()); n > 0 {
				logf("[TEMP] %d arquivo(s) temporário(s) removido(s)", n)
			}
		}
	}()

	// --- Produtor: heartbeat ------------------------------------------------------------------
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(withJitter(heartbeatInterval, heartbeatJitter, nil)):
			}
			if IsAgentUnauthorized() {
				continue
			}
			t := GetAgentToken()
			if t == "" {
				continue
			}
			if err := sendHeartbeat(ctx, GetRuntimeConfig(), t, transportLabel(tr.status())); err != nil {
				if errors.Is(err, errAgentUnauthorized) {
					go handleUnauthorized(ctx, tr, wake)
				} else {
					logf("Aviso: heartbeat: %v", err)
				}
			}
		}
	}()

	// --- Sincronização remota + supervisão do transporte ---------------------------------------
	//
	// Um único ciclo (PPE-29). O ticker de 30 min que existia aqui era **redundante** — repetia o
	// mesmo `syncDeviceConfig` do ciclo de 60 s — e era ele quem transformava a escrita no mapa de
	// config numa escrita *concorrente*.
	//
	// É este ciclo que dá ao kill-switch o "≤60 s" de PPE-10: a sync relê o bloco `events`, e o
	// supervisor liga ou desliga o transporte conforme o que voltou.
	applyTransport := func() {
		desired, enabled := eventsClientConfig(GetRuntimeConfig(), GetAgentToken())
		tr.apply(ctx, desired, enabled, wake)
	}
	applyTransport()

	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if IsAgentUnauthorized() {
				continue
			}
			t := GetAgentToken()
			if t == "" {
				continue
			}
			if err := syncDeviceConfig(ctx, t); err != nil {
				if errors.Is(err, errAgentUnauthorized) {
					go handleUnauthorized(ctx, tr, wake)
					continue // token recusado: não adianta seguir para applyTransport/sendPrinters
				}
				logf("Aviso: sync config: %v", err)
			}
			applyTransport()
			if err := sendPrinters(ctx, GetRuntimeConfig(), t); err != nil {
				logf("Aviso ao atualizar impressoras: %v", err)
			}
		}
	}()

	SetTrayOnline(true)
	<-ctx.Done()

	// PPE-30: graceful shutdown. O cancelamento já chegou a todo produtor (fallback, reaper,
	// heartbeat, sync — todos com `case <-ctx.Done(): return` no próprio select) e ao consumidor
	// (`jobs.Runner.Run` também sai no `ctx.Done()`); o que falta é dar ao job que **já estava em
	// voo** — reivindicado, imprimindo ou retentando o report final — a chance de terminar antes do
	// processo sumir. Sem isso, um job com o `printing` já reportado mas o `completed` ainda em
	// retentativa some da trilha sem nunca virar `failed`/`completed`.
	logf("Sinal de encerramento — aguardando jobs em voo (até %v)...", gracefulShutdownTimeout)
	waitForInFlightJobs(runner, gracefulShutdownTimeout)
	SetTrayOnline(false)
	logf("Agente encerrado.")
	return nil
}

// gracefulShutdownTimeout é o teto de espera por jobs em voo antes de sair de qualquer forma (PPE-30)
// — um shutdown que nunca termina é pior que perder o rastro de um job.
const gracefulShutdownTimeout = 30 * time.Second

// gracefulShutdownGrace é a margem que quem espera `run()` retornar (main(), o serviço Windows) dá
// **além** de gracefulShutdownTimeout — tempo para o próprio `waitForInFlightJobs` desistir e `run`
// terminar de escrever seu log final, sem que o teto externo dispare primeiro e mate o processo
// antes de `run` sequer ter concluído sua própria espera.
const gracefulShutdownGrace = 5 * time.Second

// waitForInFlightJobs espera `runner` esvaziar o set de jobs em voo, ou `timeout` vencer primeiro
// (PPE-30). O job que já está em voo continua seu próprio backoff de report (jobs.Runner cuida
// disso, com o context já cancelado — Report/Print recebem o mesmo ctx e podem abortar cedo); aqui
// só damos a ele a chance de terminar antes do processo sair.
func waitForInFlightJobs(runner *jobs.Runner, timeout time.Duration) {
	if runner == nil {
		return
	}
	deadline := time.After(timeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if runner.InFlight() == 0 {
			return
		}
		select {
		case <-deadline:
			logf("[SHUTDOWN] teto de %v atingido com %d job(s) ainda em voo", timeout, runner.InFlight())
			return
		case <-ticker.C:
		}
	}
}

// awaitGracefulShutdown espera `done` fechar (a goroutine que roda `run`/`runAgent` terminou de
// verdade) ou `timeout` vencer primeiro. Devolve true se terminou a tempo. É o consumidor real de
// `done` em main()/service_windows.go — separado para o teste medir o teto sem esperar
// gracefulShutdownTimeout inteiro.
func awaitGracefulShutdown(done <-chan struct{}, timeout time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// runAgent é o entrypoint do serviço Windows. `ctx` chega de fora (service_windows.go, ligado ao
// stop/shutdown do SCM) — antes desta task ele nascia aqui dentro como `context.Background()`, e
// nada do lado de fora tinha como cancelá-lo (achado (ii) da fase anterior).
func runAgent(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	SetRuntimeConfig(cfg)
	logf("Iniciando agente v%s - Servidor: %s", agentVersion, cfg.Server)
	return run(ctx, cfg)
}

func main() {
	// Abrir painel contra serviço em execução (--open-panel ou atalho do instalador).
	// Retry até 15 s para dar tempo ao serviço de subir após a instalação.
	if len(os.Args) > 1 && os.Args[1] == "--open-panel" {
		for i := 0; i < 15; i++ {
			if tryOpenGUIPanel() {
				return
			}
			time.Sleep(1 * time.Second)
		}
		// Serviço não respondeu — abre o browser mesmo assim; usuário pode recarregar.
		openBrowserToPanel()
		return
	}

	// Se o serviço já expõe o painel local, abre o navegador e encerra.
	if tryOpenGUIPanel() {
		return
	}

	// Impede segunda instância (serviço ou tray já em execução).
	if !acquireSingleInstanceMutex() {
		tryOpenGUIPanel()
		return
	}

	// Só a instância real (serviço Windows ou interativa) espelha log em disco — os atalhos de
	// painel acima nem chegam aqui. Antes de tryRunAsWindowsService: o serviço também loga.
	initFileLog()

	if tryRunAsWindowsService() {
		return
	}

	// Modo interativo: carrega config, inicia agente + tray em background e o processo principal
	// espera o sinal de encerramento (PPE-30) — em vez de bloquear para sempre em runTray/select{},
	// que engolia SIGTERM sem dar ao agente nenhuma chance de terminar um job em voo.
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Erro ao carregar config: %v\n", err)
		os.Exit(1)
	}
	SetRuntimeConfig(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := run(ctx, cfg); err != nil {
			logf("Erro no agente: %v", err)
		}
	}()

	go runTray(cfg)

	<-ctx.Done()
	logf("Sinal de encerramento recebido.")
	awaitGracefulShutdown(done, gracefulShutdownTimeout+gracefulShutdownGrace)
}
