// As rotas do painel local (PPE-19/PPE-14) vivem aqui, e não em gui_windows.go, pelo mesmo motivo
// de applyEnrollInput ter saído de lá em T19: gui_windows.go tem `//go:build windows` e o gate desta
// fase roda em Linux — código que só existe atrás da build tag é invisível ao teste. Nada aqui é
// Windows-específico: `net/http`, `crypto/rand`, `crypto/subtle` são todos multiplataforma. Só quem
// liga o listener (`net.Listen` na porta fixa) continua em gui_windows.go, porque o painel em si
// (ligar automaticamente, abrir o navegador) é uma decisão de produto restrita ao Windows.
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// GUISessionHeader é o header que toda rota `/api/*` do painel local exige (PPE-19,
// diagnostic.md §3 achado #6).
const GUISessionHeader = "X-Agent-Session"

// newGUISessionToken gera um segredo de 32 bytes (256 bits) por boot — nunca persistido, nunca
// reusado entre reinícios. É o que fecha o CSRF do achado #6: antes, qualquer página aberta no
// mesmo navegador podia falar com `/api/*` só por conhecer a porta fixa (127.0.0.1:17345), sem
// nenhum segredo — inclusive `/api/enroll`, que troca o servidor e a chave de enrollment do device.
func newGUISessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("gerar token de sessão do painel: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// guiAllowedHost é o único host que o painel aceita. Fixo de propósito: o painel não é servido em
// nenhum outro endereço (`startFixedGUIServer` liga só em 127.0.0.1:<fixedGUIPort>), então qualquer
// `Host` diferente é, por definição, uma requisição que não veio de quem abriu essa página.
func guiAllowedHost() string {
	return fmt.Sprintf("127.0.0.1:%d", fixedGUIPort)
}

// validGUIRequest reporta se a requisição pode ter vindo da própria página do painel.
//
// `Host` errado é sempre recusado. `Origin`, quando presente, precisa bater com o painel — mas
// `Origin` ausente é aceito: é o caso normal de navegação direta (GET de topo, não é uma
// requisição cross-origin) e de clientes HTTP que simplesmente não mandam esse header. A defesa
// contra CSRF não depende de exigir Origin: um `fetch` cross-origin que tentasse mandar o header de
// sessão (que a página atacante não conhece) já dispara preflight CORS, que este servidor nunca
// aprova por não devolver `Access-Control-Allow-Origin` nenhum.
func validGUIRequest(r *http.Request) bool {
	if r.Host != guiAllowedHost() {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	return origin == "http://"+guiAllowedHost()
}

// secureCompareStrings compara em tempo constante — um segredo de sessão comparado por `==` vaza
// quantos bytes iniciais batem através do tempo de resposta.
func secureCompareStrings(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// requireGUISession recusa (403) qualquer requisição sem o header de sessão certo, ou com
// Origin/Host que não é o do painel (PPE-19).
//
// `/` fica de fora desta guarda por necessidade, não por descuido: é a própria página que entrega o
// token ao navegador (`renderGUIHTML`), e nada consegue mandar o header antes de ter lido o token —
// ela ainda passa por `validGUIRequest` para o Host/Origin.
func requireGUISession(sessionToken string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validGUIRequest(r) {
			http.Error(w, "origem não permitida", http.StatusForbidden)
			return
		}
		got := r.Header.Get(GUISessionHeader)
		if got == "" || !secureCompareStrings(got, sessionToken) {
			http.Error(w, "sessão inválida", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// maskToken mascara um segredo para exibição (PPE-14/PPE-19): só os 4 primeiros e 4 últimos
// caracteres sobrevivem — o suficiente para o usuário reconhecer "é este device", nunca para
// reconstruir o valor. Token vazio continua vazio (é o estado "ainda não pareado" que a UI já trata
// à parte); token curto demais para a máscara de 4+4 vira pontos, nunca o valor cru.
func maskToken(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	if len(token) <= 8 {
		return strings.Repeat("•", len(token))
	}
	return token[:4] + "…" + token[len(token)-4:]
}

// renderGUIHTML injeta o token de sessão do boot na página (PPE-19): é dali que o JavaScript lê o
// valor para devolvê-lo em `X-Agent-Session` em toda chamada a `/api/*`. O token é hex
// (`newGUISessionToken`), então a substituição direta dentro de uma string literal JS é segura —
// não há aspas nem caractere de controle para escapar.
func renderGUIHTML(sessionToken string) string {
	return strings.ReplaceAll(guiHTML, guiSessionPlaceholder, sessionToken)
}

// maskEnrollmentKey mascara a chave de enrollment da mesma forma que maskToken — os dois segredos
// do painel (chave de conta e token de device) nunca aparecem íntegros numa resposta HTTP.
func maskEnrollmentKey(cfg *Config) string {
	if cfg == nil {
		return ""
	}
	return maskToken(cfg.EnrollmentKey)
}

// registerGUIRoutes monta as rotas do painel local. `sessionToken` é o segredo do boot: toda rota
// `/api/*` passa por requireGUISession; `/` fica fora dela mas ainda valida Origin/Host.
func registerGUIRoutes(mux *http.ServeMux, initialCfg *Config, sessionToken string) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if !validGUIRequest(r) {
			http.Error(w, "origem não permitida", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(renderGUIHTML(sessionToken)))
	})

	mux.HandleFunc("/api/status", requireGUISession(sessionToken, func(w http.ResponseWriter, r *http.Request) {
		cfg := GetRuntimeConfig()
		if cfg == nil {
			cfg = initialCfg
		}
		token := GetAgentToken()
		online := IsAgentOnline()
		statusStr := "Aguardando conexão..."
		if online {
			statusStr = "Conectado"
		}
		pairErr := GetLastPairError()
		if pairErr != "" && token == "" {
			statusStr = "Erro de vinculação"
		}
		// PPE-20: o estado "Desvinculado" existia só internamente (IsAgentUnauthorized()) — o
		// mecanismo de revogação funcionava (401 → transportes param → re-pair único), mas nunca
		// chegava ao usuário. Ganha prioridade sobre online/pairErr: é o estado mais específico e o
		// único que explica por que o agente parou de vez (achado 2 do gate spec-driven-eval de
		// 2026-08-10).
		unauthorized := IsAgentUnauthorized()
		if unauthorized {
			statusStr = "Desvinculado"
		}

		printers, _ := getPrinters()
		type printerInfo struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Msg    string `json:"msg"`
		}
		var ps []printerInfo
		for _, p := range printers {
			ps = append(ps, printerInfo{
				Name:   p["name"],
				Status: p["status"],
				Msg:    p["statusMessage"],
			})
		}

		hasKey := cfg != nil && strings.TrimSpace(cfg.EnrollmentKey) != ""
		server := ""
		name := GetAgentName()
		if cfg != nil {
			server = cfg.Server
			if name == "" {
				name = cfg.Name
			}
		}

		tokenFile := ""
		if cfg != nil {
			tokenFile = cfg.TokenFile
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"agentName": name,
			"version":   agentVersion,
			// PPE-14/PPE-19: mascarado — antes, o token íntegro do device saía em texto puro nesta
			// resposta, alcançável por qualquer processo que soubesse a porta fixa (achado #6).
			"token":            maskToken(token),
			"status":           statusStr,
			"online":           online,
			"unauthorized":     unauthorized,
			"printers":         ps,
			"server":           server,
			"hasEnrollmentKey": hasKey,
			"enrollmentKey":    maskEnrollmentKey(cfg),
			"paired":           token != "",
			"pairError":        pairErr,
			"configPath":       configFilePath(),
			"dataDir":          configDataDir(),
			"tokenFile":        tokenFile,
		})
	}))

	mux.HandleFunc("/api/rename", requireGUISession(sessionToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
			http.Error(w, "nome inválido", http.StatusBadRequest)
			return
		}
		if GetRuntimeConfig() == nil {
			http.Error(w, "config indisponível", http.StatusInternalServerError)
			return
		}
		name := strings.TrimSpace(body.Name)
		cfg := UpdateRuntimeConfig(func(c *Config) { c.Name = name })
		SetAgentName(cfg.Name)
		// Clone: saveConfig normaliza (muta) o que recebe, e cfg é a config publicada.
		if err := saveConfig(cfg.Clone()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if token := GetAgentToken(); token != "" {
			if err := patchDeviceName(r.Context(), cfg, token, cfg.Name); err != nil {
				logf("Aviso: rename remoto: %v", err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true", "name": cfg.Name})
	}))

	mux.HandleFunc("/api/enroll", requireGUISession(sessionToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Name          string `json:"name"`
			Server        string `json:"server"`
			EnrollmentKey string `json:"enrollmentKey"`
			RePair        bool   `json:"rePair"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "corpo inválido", http.StatusBadRequest)
			return
		}
		if GetRuntimeConfig() == nil {
			http.Error(w, "config indisponível", http.StatusInternalServerError)
			return
		}
		// A validação roda sobre um candidato local: config inválida nunca chega a ser publicada.
		candidate := GetRuntimeConfig().Clone()
		applyEnrollInput(candidate, strings.TrimSpace(body.Name), strings.TrimSpace(body.Server), strings.TrimSpace(body.EnrollmentKey))
		if strings.TrimSpace(candidate.EnrollmentKey) == "" {
			http.Error(w, "enrollmentKey é obrigatório", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(candidate.Name) == "" {
			http.Error(w, "name é obrigatório", http.StatusBadRequest)
			return
		}
		name, server, key := candidate.Name, candidate.Server, candidate.EnrollmentKey
		cfg := UpdateRuntimeConfig(func(c *Config) { applyEnrollInput(c, name, server, key) })
		SetAgentName(cfg.Name)
		if err := saveConfig(cfg.Clone()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		var tokenStr string
		var err error
		if body.RePair || GetAgentToken() == "" {
			tokenStr, err = rePair(r.Context(), cfg)
		} else {
			tokenStr = GetAgentToken()
		}
		if err != nil {
			SetLastPairError(err.Error())
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		SetLastPairError("")
		SetTrayToken(tokenStr)
		// PPE-20: um re-pareamento manual bem-sucedido tira o agente do estado "Desvinculado" —
		// sem isto, os produtores de tráfego (poll/heartbeat/sync) continuariam de braços cruzados
		// mesmo com um token novo e válido em mãos, porque IsAgentUnauthorized() ainda diria true.
		SetAgentUnauthorized(false)
		w.Header().Set("Content-Type", "application/json")
		// PPE-14: mascarado pelo mesmo motivo do /api/status — o corpo desta resposta não precisa
		// do token íntegro para confirmar ao usuário que o pareamento funcionou.
		_ = json.NewEncoder(w).Encode(map[string]string{
			"ok":    "true",
			"token": maskToken(tokenStr),
			"name":  cfg.Name,
		})
	}))
}

// guiSessionPlaceholder é substituído pelo token de sessão do boot em renderGUIHTML.
const guiSessionPlaceholder = "%%AGENT_SESSION%%"

const guiHTML = `<!DOCTYPE html>
<html lang="pt-BR">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Impressão Remota</title>
<style>
  *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
  :root {
    --bg: #0f172a; --surface: #1e293b; --surface2: #273549; --border: #334155;
    --accent: #3b82f6; --accent-hover: #2563eb; --green: #22c55e; --red: #ef4444;
    --yellow: #f59e0b; --text: #f1f5f9; --muted: #94a3b8; --radius: 10px;
  }
  body { background: var(--bg); color: var(--text); font-family: 'Segoe UI', system-ui, sans-serif;
    min-height: 100vh; display: flex; align-items: flex-start; justify-content: center; padding: 32px 16px; }
  .container { width: 100%; max-width: 560px; display: flex; flex-direction: column; gap: 20px; }
  .header { display: flex; align-items: center; gap: 14px; }
  .logo { width: 44px; height: 44px; background: var(--accent); border-radius: 10px;
    display: flex; align-items: center; justify-content: center; flex-shrink: 0; }
  .logo svg { width: 24px; height: 24px; fill: white; }
  .header-text h1 { font-size: 1.25rem; font-weight: 700; }
  .header-text p { font-size: 0.8rem; color: var(--muted); margin-top: 2px; }
  .card { background: var(--surface); border: 1px solid var(--border); border-radius: var(--radius); padding: 20px; }
  .card-title { font-size: 0.75rem; font-weight: 600; text-transform: uppercase; letter-spacing: .06em;
    color: var(--muted); margin-bottom: 14px; }
  .hint { font-size: 0.78rem; color: var(--muted); margin-bottom: 12px; line-height: 1.45; }
  .field { display: flex; flex-direction: column; gap: 6px; margin-bottom: 12px; }
  .field label { font-size: 0.78rem; color: var(--muted); font-weight: 600; }
  .input-field { width: 100%; background: var(--surface2); border: 1px solid var(--border); border-radius: 7px;
    padding: 9px 13px; color: var(--text); font-size: 0.9rem; outline: none; }
  .input-field:focus { border-color: var(--accent); }
  .status-row { display: flex; align-items: center; gap: 10px; }
  .dot { width: 10px; height: 10px; border-radius: 50%; flex-shrink: 0; background: var(--muted); }
  .dot.online { background: var(--green); box-shadow: 0 0 6px var(--green); }
  .dot.offline { background: var(--red); }
  .dot.unauthorized { background: var(--yellow); box-shadow: 0 0 6px var(--yellow); }
  .hint.unauthorized-hint { color: var(--yellow); margin-top: 8px; }
  .name-row { display: flex; gap: 10px; align-items: center; }
  .name-row .input-field { flex: 1; }
  .btn { display: inline-flex; align-items: center; justify-content: center; gap: 6px;
    padding: 8px 16px; border-radius: 7px; border: none; font-size: 0.85rem; font-weight: 600;
    cursor: pointer; transition: background .15s; }
  .btn-primary { background: var(--accent); color: white; }
  .btn-primary:hover { background: var(--accent-hover); }
  .btn-secondary { background: var(--surface2); color: var(--text); border: 1px solid var(--border); }
  .feedback { font-size: 0.78rem; margin-top: 8px; min-height: 18px; }
  .feedback.ok { color: var(--green); }
  .feedback.err { color: var(--red); }
  .token-box { background: var(--surface2); border: 1px solid var(--border); border-radius: 8px;
    padding: 12px 14px; font-family: monospace; font-size: 0.82rem; word-break: break-all; }
  .token-box.empty { color: var(--muted); font-style: italic; font-family: inherit; }
  .printer-list { display: flex; flex-direction: column; gap: 8px; }
  .printer-item { background: var(--surface2); border: 1px solid var(--border); border-radius: 8px;
    padding: 10px 14px; display: flex; align-items: center; gap: 12px; }
  .printer-name { font-size: 0.88rem; font-weight: 500; flex: 1; }
  .badge { font-size: 0.72rem; font-weight: 600; padding: 2px 8px; border-radius: 20px; text-transform: uppercase; }
  .badge-ready { background: rgba(34,197,94,.15); color: var(--green); }
  .badge-offline { background: rgba(239,68,68,.15); color: var(--red); }
  .badge-unknown { background: rgba(148,163,184,.15); color: var(--muted); }
  .footer { text-align: center; color: var(--muted); font-size: 0.75rem; }
  .spinner { display: inline-block; width: 14px; height: 14px; border: 2px solid var(--border);
    border-top-color: var(--accent); border-radius: 50%; animation: spin .7s linear infinite; }
  @keyframes spin { to { transform: rotate(360deg); } }
</style>
</head>
<body>
<div class="container">
  <div class="header">
    <div class="logo"><svg viewBox="0 0 24 24"><path d="M6 2h12v5H6V2zm-2 7h16a2 2 0 0 1 2 2v5a2 2 0 0 1-2 2h-2v3H6v-3H4a2 2 0 0 1-2-2v-5a2 2 0 0 1 2-2zm2 9v2h12v-2H6zm10-6a1 1 0 1 0 0-2 1 1 0 0 0 0 2z"/></svg></div>
    <div class="header-text">
      <h1 id="agent-name">Impressão Remota</h1>
      <p id="agent-version">v—</p>
    </div>
  </div>

  <div class="card">
    <div class="card-title">Status da Conexão</div>
    <div class="status-row">
      <div class="dot" id="status-dot"></div>
      <span id="status-text">Carregando...</span>
    </div>
    <p class="hint unauthorized-hint" id="unauthorized-hint" style="display:none">Vinculação recusada pelo servidor — vincule novamente abaixo para voltar a imprimir.</p>
    <p class="feedback err" id="pair-error"></p>
  </div>

  <div class="card">
    <div class="card-title">Vinculação à Conta</div>
    <p class="hint">Copie a chave em <strong>Configurações da Conta → Impressão</strong>. O campo <strong>URL da API</strong> deve ser o mesmo ambiente do app (prod ou devel).</p>
    <div class="field">
      <label for="server-input">URL da API (server)</label>
      <input class="input-field" id="server-input" type="url" placeholder="https://api.pedagogicoonline.com.br" />
    </div>
    <div class="field">
      <label for="key-input">Chave de enrollment</label>
      <input class="input-field" id="key-input" type="text" placeholder="pm_..." autocomplete="off" />
    </div>
    <div class="field">
      <label for="name-input">Nome do agente</label>
      <div class="name-row">
        <input class="input-field" id="name-input" type="text" placeholder="Ex.: Sala Direção" />
        <button class="btn btn-secondary" id="save-btn">Salvar</button>
      </div>
      <div class="feedback" id="save-feedback"></div>
    </div>
    <button class="btn btn-primary" id="enroll-btn" style="width:100%">Vincular / Re-vincular</button>
    <div class="feedback" id="enroll-feedback"></div>
  </div>

  <div class="card">
    <div class="card-title">Token de Autenticação</div>
    <div class="token-box empty" id="token-value">Aguardando pareamento...</div>
  </div>

  <div class="card">
    <div class="card-title">Impressoras Disponíveis</div>
    <p class="hint">Tipos (A4/Térmica) são definidos no admin web, aba Impressoras.</p>
    <div class="printer-list" id="printer-list"><div class="no-printers"><span class="spinner"></span></div></div>
  </div>

  <div class="card" style="font-size:11px;color:var(--muted);word-break:break-all">
    <div class="card-title" style="font-size:12px">Pasta de dados</div>
    <div id="data-dir-path">—</div>
  </div>

  <div class="footer" id="footer-version"></div>
</div>
<script>
const AGENT_SESSION = "` + guiSessionPlaceholder + `";
const badgeClass = { ready: 'badge-ready', offline: 'badge-offline', unknown: 'badge-unknown' };
const badgeLabel = { ready: 'Pronta', offline: 'Offline', unknown: 'Desconhecida' };
let settingsDirty = false;

function escHtml(s) {
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
}

async function loadStatus() {
  try {
    const r = await fetch('/api/status', { headers: { 'X-Agent-Session': AGENT_SESSION } });
    const d = await r.json();
    document.getElementById('agent-name').textContent = d.agentName || 'Impressão Remota';
    document.getElementById('agent-version').textContent = 'v' + (d.version || '—');
    document.getElementById('footer-version').textContent = 'Impressão Remota v' + (d.version || '—');
    const dot = document.getElementById('status-dot');
    // PPE-20: "Desvinculado" (d.unauthorized) tem prioridade visual sobre online/offline — é o único
    // estado que explica por que o agente parou de vez, em vez de só estar temporariamente sem rede.
    dot.className = 'dot ' + (d.unauthorized ? 'unauthorized' : (d.online ? 'online' : 'offline'));
    document.getElementById('status-text').textContent = d.status;
    document.getElementById('unauthorized-hint').style.display = d.unauthorized ? 'block' : 'none';
    document.getElementById('pair-error').textContent = d.pairError || '';
    const tokenEl = document.getElementById('token-value');
    if (d.token) {
      tokenEl.textContent = d.token;
      tokenEl.classList.remove('empty');
    } else {
      tokenEl.textContent = 'Aguardando pareamento...';
      tokenEl.classList.add('empty');
    }
    if (!settingsDirty) {
      document.getElementById('server-input').value = d.server || '';
      document.getElementById('name-input').value = d.agentName || '';
    }
    const list = document.getElementById('printer-list');
    if (!d.printers || d.printers.length === 0) {
      list.innerHTML = '<div style="color:var(--muted);text-align:center;padding:12px 0">Nenhuma impressora encontrada</div>';
    } else {
      list.innerHTML = d.printers.map(p => {
        const cls = badgeClass[p.status] || 'badge-unknown';
        const lbl = badgeLabel[p.status] || p.status;
        return '<div class="printer-item"><span class="printer-name">' + escHtml(p.name) + '</span><span class="badge ' + cls + '">' + lbl + '</span></div>';
      }).join('');
    }
    const dataDirEl = document.getElementById('data-dir-path');
    if (dataDirEl) dataDirEl.textContent = d.dataDir || '—';
  } catch(e) {
    document.getElementById('status-text').textContent = 'Erro ao carregar dados';
  }
}

['server-input','key-input','name-input'].forEach(id => {
  document.getElementById(id).addEventListener('input', () => { settingsDirty = true; });
});

document.getElementById('enroll-btn').addEventListener('click', async () => {
  const fb = document.getElementById('enroll-feedback');
  const btn = document.getElementById('enroll-btn');
  const payload = {
    server: document.getElementById('server-input').value.trim(),
    enrollmentKey: document.getElementById('key-input').value.trim(),
    name: document.getElementById('name-input').value.trim(),
    rePair: true
  };
  if (!payload.server || !payload.enrollmentKey || !payload.name) {
    fb.textContent = 'Preencha URL da API, chave e nome.';
    fb.className = 'feedback err';
    return;
  }
  btn.disabled = true;
  btn.innerHTML = '<span class="spinner"></span> Vinculando...';
  fb.textContent = '';
  try {
    const r = await fetch('/api/enroll', { method: 'POST', headers: {'Content-Type':'application/json', 'X-Agent-Session': AGENT_SESSION}, body: JSON.stringify(payload) });
    const txt = await r.text();
    if (r.ok) {
      fb.textContent = 'Agente vinculado com sucesso!';
      fb.className = 'feedback ok';
      settingsDirty = false;
      loadStatus();
    } else {
      fb.textContent = txt || 'Falha ao vincular.';
      fb.className = 'feedback err';
    }
  } catch(e) {
    fb.textContent = 'Erro de conexão com o agente.';
    fb.className = 'feedback err';
  }
  btn.disabled = false;
  btn.textContent = 'Vincular / Re-vincular';
});

document.getElementById('save-btn').addEventListener('click', async () => {
  const name = document.getElementById('name-input').value.trim();
  const fb = document.getElementById('save-feedback');
  if (!name) { fb.textContent = 'O nome não pode ser vazio.'; fb.className = 'feedback err'; return; }
  const btn = document.getElementById('save-btn');
  btn.disabled = true;
  try {
    const r = await fetch('/api/rename', { method: 'POST', headers: {'Content-Type':'application/json', 'X-Agent-Session': AGENT_SESSION}, body: JSON.stringify({name}) });
    if (r.ok) {
      fb.textContent = 'Nome salvo.';
      fb.className = 'feedback ok';
      document.getElementById('agent-name').textContent = name;
    } else {
      fb.textContent = 'Erro: ' + (await r.text());
      fb.className = 'feedback err';
    }
  } catch(e) {
    fb.textContent = 'Erro de conexão.';
    fb.className = 'feedback err';
  }
  btn.disabled = false;
});

loadStatus();
setInterval(loadStatus, 5000);
</script>
</body>
</html>`
