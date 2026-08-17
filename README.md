# Agente de Impressão — Gestor Escolar

> **Sobre este repositório.** Este é o código-fonte aberto do agente de impressão do
> [Gestor Escolar](https://app.pedagogicoonline.com.br), um ERP escolar. O agente roda na máquina da
> escola: descobre impressoras locais, recebe jobs do backend e imprime. O ERP em si é privado — só o
> agente é aberto, sob licença [MIT](LICENSE).
>
> Hoje o código é espelhado do monorepo privado, que continua sendo quem compila e publica os
> releases oficiais. `./build.sh` sozinho gera os artefatos em `dist/` e para — os passos de
> publicação no app web não se aplicam a quem builda só a partir daqui.

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

## Build

```bash
./build.sh
```

Gera em `dist/`:

| Saída | Uso |
|-------|-----|
| `gestor-escolar.exe` | binário Windows x86 |
| `GestorEscolar-Setup-{VERSION}.exe` | instalador Windows (Inno Setup) |
| `gestor-escolar-linux-arm64-{VERSION}.tar.gz` | instalador Linux ARM64 |

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
típica 1–5 dias úteis, propagação 24–48 h).

Este projeto é candidato ao programa de certificado gratuito da [SignPath Foundation](https://signpath.org)
para software open source — aplicação em andamento.

## Distribuição

Releases oficiais (com o Windows assinado, quando disponível) são publicados como
[GitHub Releases](../../releases) deste repositório ou espelhados a partir do monorepo privado que
compila e serve o app web. Tag: `print-agent-v<VERSION>` (de `print-agent/VERSION`); asset:
`print-agent-public.zip`.

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

## Manifesto de auto-update

`update.go` consulta `{appUrl}/print-agent-version.json` periodicamente. Formato:

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

O `build.sh` gera esse manifesto automaticamente com o SHA-256 dos artefatos.
