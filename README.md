# Agente de Impressão — Gestor Escolar

> **Sobre este repositório.** Este é o código-fonte aberto do agente de impressão do
> [Gestor Escolar](https://app.pedagogicoonline.com.br), um ERP escolar. O agente roda na máquina da
> escola: descobre impressoras locais, recebe jobs do backend e imprime. O ERP em si é privado — só o
> agente é aberto, sob licença [MIT](LICENSE).
>
> Hoje o código é espelhado do monorepo privado, que continua sendo quem compila e publica os
> releases. Passos marcados como "monorepo" abaixo (publicar no app web, `npm run pack:print-agent`)
> não se aplicam a quem builda só a partir daqui — `./build.sh` gera os artefatos em `dist/` e para.

Agente local (Windows/Linux) que descobre impressoras, faz polling de jobs no backend e imprime PDFs via SumatraPDF (Windows) ou `lp` (Linux).

## Pré-requisitos

- Go 1.22+
- Windows: SumatraPDF (opcional, recomendado para PDF)
- Linux: CUPS (`lp`, `lpstat`)
- Docker (instalador Windows via `amake/innosetup`) ou Inno Setup local
- Para gerar o instalador Windows: `print-agent/SumatraPDF.exe` presente em disco (não é mais
  versionado no git — ver nota abaixo). Baixe a versão portátil em
  [sumatrapdfreader.org](https://www.sumatrapdfreader.org/download-free-pdf-viewer) e salve como
  `print-agent/SumatraPDF.exe`. Sem o arquivo, `./build.sh` falha explicitamente antes de chamar o
  Inno Setup (em vez do erro genérico "source file not found" do ISCC).

## Configuração

O agente grava configuração em **`%ProgramData%\GestorEscolar\`** (Windows) ou **`/var/lib/gestor-escolar/`** (Linux). Copie [`config.json.example`](config.json.example) ou use o painel web local.

```json
{
  "server": "https://api.pedagogicoonline.com.br",
  "appUrl": "https://app.pedagogicoonline.com.br",
  "name": "Agente Escola Municipal",
  "tokenFile": "token.txt",
  "enrollmentKey": "<print_api_key da conta>",
  "sumatraPdfPath": "SumatraPDF.exe",
  "printSettings": "fit"
}
```

- `server`: URL da **API** — **deve ser o mesmo ambiente** do app onde você abre Configurações → Impressão (prod vs devel)
- `appUrl`: URL do app web (opcional; derivada de `server` se omitida)
- `enrollmentKey`: chave em **Configurações da Conta → Impressão**
- **Tipos de impressora (A4/Térmica):** configure no admin web, aba Impressoras — **não** no `config.json` local
- **Painel local (Windows):** http://127.0.0.1:17345 — vinculação, nome e status (funciona com serviço `GestorEscolar`)

Problemas comuns: [`TROUBLESHOOTING.md`](TROUBLESHOOTING.md)

## Build e publicação

```bash
cd print-agent
./build.sh
cd ../frontend
npm run pack:print-agent
```

Gera em `dist/` e publica em `frontend/public/`:

| Saída | Uso |
|-------|-----|
| `dist/gestor-escolar.exe` | build local |
| `dist/GestorEscolar-Setup-{VERSION}.exe` | instalador Windows |
| `dist/gestor-escolar-linux-arm64-{VERSION}.tar.gz` | Linux ARM64 |
| `frontend/public/print-agent/*` | download estático no app |
| `frontend/public/print-agent-version.json` | manifest auto-update (SHA-256) |
| `releases/print-agent-public.zip` | bundle local (não versionado — ver Distribuição) |

O Amplify **não** compila Go. O `prebuild` do frontend descompacta `releases/print-agent-public.zip`
via `frontend/scripts/unpack-print-agent-bundle.mjs` (no monorepo).

Detalhes do instalador: [`installer/README.md`](installer/README.md).

### Assinatura de código (Authenticode)

Os `.exe` publicados hoje **não são assinados**. Consequência concreta, observada em campo (2026-08-16):
o **Smart App Control** do Windows 11 (ativo só em instalação limpa 22H2+) bloqueia
`GestorEscolar-Setup-{VERSION}.exe` e **não oferece bypass por arquivo** — a única saída na máquina é
desligar o SAC, o que é **irreversível sem reinstalar o Windows**. O mesmo vale para o `.exe` que o
auto-update ([`update.go`](update.go)) baixa: SAC bloqueia a execução do binário novo.

O `build.sh` já assina quando o certificado existe; sem as variáveis abaixo o comportamento é
idêntico ao de antes (build segue, artefatos sem assinatura, aviso no console).

| Variável | Efeito |
|---|---|
| `CODESIGN_CMD` | Comando que recebe o `.exe` como último argumento e assina in-place. Caminho para HSM/token: `signtool`, `AzureSignTool`, `jsign`, `osslsigncode` com engine PKCS#11 |
| `CODESIGN_PFX` / `CODESIGN_PFX_PASS` | Assina via `osslsigncode` com `.pfx`. Só serve para certificado legado — desde jun/2023 a baseline CA/B exige chave em token/HSM |
| `CODESIGN_TIMESTAMP_URL` | Servidor RFC3161 (default `http://timestamp.digicert.com`) |
| `CODESIGN_ISCC_SIGNTOOL` | Manda o **próprio Inno Setup** assinar setup + desinstalador (`SignTool` + `SignedUninstaller`). Único jeito de assinar `unins000.exe`, que é gerado de um stub embutido. Exige ISCC nativo ou Wine — no caminho Docker o build **falha explicitamente** em vez de gerar desinstalador não assinado em silêncio |

```bash
# exemplo (token/HSM via signtool em host Windows/Wine)
export CODESIGN_CMD='signtool.exe sign /fd SHA256 /tr http://timestamp.digicert.com /td SHA256 /a'
export CODESIGN_ISCC_SIGNTOOL='signtool.exe sign /fd SHA256 /tr http://timestamp.digicert.com /td SHA256 /a $f'
./build.sh
```

Ordem no build: `gestor-escolar.exe` é assinado **antes** do Inno empacotar (senão o binário instalado
e o do auto-update sairiam sem assinatura), e o SHA-256 do manifest é calculado depois — o
`print-agent-version.json` já reflete o binário assinado.

**Assinar não basta sozinho.** SAC exige assinatura válida **e** reputação positiva no ISG da
Microsoft, e desde 2024 nem certificado EV concede reputação imediata — cada hash novo começa do
zero. Para um app de baixo volume como este, o caminho prático é submeter cada release ao
[WDSI](https://www.microsoft.com/en-us/wdsi/filesubmission) como *software developer* (resposta
típica 1–5 dias úteis, propagação 24–48 h). Escolha de CA e custos: ver `BACKLOG.md`.

### Distribuição (034-print-push-events/T29, PPE-32)

O zip **não é mais versionado no git** (era ~15 MB por release; somado a `SumatraPDF.exe` e a builds
soltos, ~56 MB rastreados no índice antes desta task). A origem passou a ser **GitHub Releases**:

- **Tag**: `print-agent-v<VERSION>` (ex.: `print-agent-v2.2.0`, de `print-agent/VERSION`).
- **Asset**: `print-agent-public.zip` — o mesmo arquivo que `npm run pack:print-agent` produz em
  `releases/print-agent-public.zip`.
- **Repo**: `ZoglioControllato/gestor-escolar` (default de `unpack-print-agent-bundle.mjs`;
  sobrescrevível por `PRINT_AGENT_RELEASES_REPO`, ou a URL inteira por `PRINT_AGENT_BUNDLE_URL`).

**Passo de ops (não automatizado por este pipeline — sem `gh` CLI autenticado neste ambiente)**: após
`npm run pack:print-agent`, publique o zip como asset do release:

```bash
# da raiz do repositório, depois de ./build.sh && npm run pack:print-agent --prefix frontend
gh release create "print-agent-v$(cat print-agent/VERSION)" \
  print-agent/releases/print-agent-public.zip \
  --repo ZoglioControllato/gestor-escolar \
  --title "print-agent v$(cat print-agent/VERSION)"
```

O `prebuild` do frontend (`unpack-print-agent-bundle.mjs`) baixa esse asset quando o zip local está
ausente — é o caso normal em CI/Amplify Hosting, que faz checkout limpo. Se o download falhar em CI
(release ainda não publicado, tag errada, repo errado), o `prebuild` **falha explicitamente** com a
URL tentada e a instrução acima — nunca produz um build com artefatos do agente vazios/ausentes em
silêncio. Fora de CI (dev local), a falha de download só avisa e segue (ergonomia local — o app roda
sem os binários do agente).

Limpeza do **histórico** do git (o zip e os outros binários continuam nos commits antigos, junto com
o token versionado) permanece fora de escopo — ver BACKLOG:126.

## Execução

```bash
./dist/gestor-escolar
```

1. `POST /print-agent/pair` (se não houver token)
2. Impressoras a cada 60s → `POST /print-agent/printers`
3. Polling jobs 1s → `GET /print-agent/pending-jobs`
4. Imprime PDF via presigned URL S3
5. Auto-update (Windows): `GET {appUrl}/print-agent-version.json` → download `/print-agent/*.exe`

## API backend (`/print-agent/*`)

Contrato dos endpoints consumidos pelo agente:

| Método | Path |
|--------|------|
| POST | `/print-agent/pair` |
| GET | `/print-agent/pending-jobs` |
| POST | `/print-agent/job-status` |
| POST | `/print-agent/printers` |
| GET | `/print-agent/device-config` |
| PATCH | `/print-agent/device` |

Autenticação (exceto `pair`): `Authorization: Bearer {device_token}`

> `POST /print-agent/device-config/ack` existiu e foi removida (034-print-push-events/T30, PPE-33):
> era o outro lado de um mecanismo de cache por ETag que o servidor nunca implementou de verdade —
> nunca mandava `etag` no `GET /device-config` acima, então o agente nunca tinha motivo real para
> chamar o ack.

## Estático no app web (`/print-agent/*`)

Mesma estratégia do `/version.json` do app web:

- **`/print-agent-version.json`** — versão + URLs relativas + SHA-256 (gerado pelo `build.sh`)
- **`/print-agent/gestor-escolar-{VERSION}.exe`** — auto-update Windows x86
- **`/print-agent/GestorEscolar-Setup-{VERSION}.exe`** — instalador manual
- **`/print-agent/gestor-escolar-linux-arm64-{VERSION}.tar.gz`** — Linux ARM64

Exemplo de manifest gerado:

```json
{
  "version": "2.0.1",
  "artifacts": {
    "386": { "url": "/print-agent/gestor-escolar-2.0.1.exe", "sha256": "…" },
    "arm64": { "url": "/print-agent/gestor-escolar-linux-arm64-2.0.1.tar.gz", "sha256": "…" }
  },
  "setup": { "386": "/print-agent/GestorEscolar-Setup-2.0.1.exe" }
}
```

Release: bump [`VERSION`](VERSION) → `./build.sh` → `npm run pack:print-agent --prefix ../frontend` →
publicar o zip como GitHub Release (ver § Distribuição acima) → deploy Amplify.
