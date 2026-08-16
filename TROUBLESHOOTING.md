# Print-agent — solução de problemas

## Onde está o log do agente (primeiro lugar a olhar)

A partir do **2.2.2**, tudo que o agente loga vai também para um arquivo em disco (o serviço do
Windows descarta o stdout — antes dessa versão não havia log local nenhum):

| Sistema | Caminho |
|---------|---------|
| Windows | `C:\ProgramData\GestorEscolar\agent.log` |
| Linux   | `/var/lib/gestor-escolar/agent.log` |

Rotação automática em 5 MB — a história anterior fica em `agent.log.1` (só 1 backup; o par nunca
passa de ~10 MB). Linhas úteis para diagnóstico: `[EVENTS]` (WebSocket/push — conexão, assinatura,
recusa de autorização, reconexão), update automático e erros de impressão. Se o arquivo não
existir, o agente não teve permissão de escrita no diretório — ele segue funcionando e avisa uma
única vez no stdout.

## Agente não aparece na conta (ex.: Sala Direção)

### Causa mais comum: ambiente errado

O campo `server` do agente **deve apontar para a mesma API** do app onde você abre **Configurações da Conta → Impressão**.

| App (navegador) | `server` no config |
|-----------------|-------------------|
| `app.pedagogicoonline.com.br` (produção) | `https://api.pedagogicoonline.com.br` |
| `app-devel.pedagogicoonline.com.br` | `https://api-devel.pedagogicoonline.com.br` |

Se o agente pareia em **devel** e você consulta o admin em **produção**, o dispositivo **não aparecerá**.

### Procedimento de re-vinculação (Windows, serviço GestorEscolar)

Execute **PowerShell como Administrador**:

```powershell
sc stop GestorEscolar
$configDir = "$env:ProgramData\GestorEscolar"
# Edite config.json (server, enrollmentKey, name) ou use o painel em http://127.0.0.1:17345
Remove-Item "$configDir\token.txt" -ErrorAction SilentlyContinue
sc start GestorEscolar
Start-Process "http://127.0.0.1:17345"
```

1. Copie a chave de enrollment em **Configurações da Conta → Impressão** (mesmo ambiente do `server`).
2. No painel local, aba **Vinculação**, informe `server`, chave e nome do agente.
3. Clique **Vincular / Re-vincular**.
4. No admin web, aba **Dispositivos**, procure pelo **nome do agente** (não pelo hostname Windows).
5. Aba **Impressoras** → habilitar cada impressora e definir tipo **A4** ou **Térmica**.

### Outras causas

- **`token.txt` antigo** — impede novo pareamento; apague o arquivo antes de re-vincular.
- **Chave regenerada no admin** — agentes já pareados mantêm token; novos installs precisam da chave nova + apagar `token.txt`.
- **Usuário não é admin** — só administradores veem a aba Dispositivos.
- **Conta errada no seletor** — a lista filtra pela conta ativa no app.

## Nome salvo no painel não persiste

A partir da v2.0.2+, `config.json` fica em **`%ProgramData%\GestorEscolar\`** (não na pasta do executável em Program Files). Se o save falhar, o painel exibe erro em vermelho.

## Tipos de impressora

Configure tipos **somente** em **Configurações da Conta → Impressão → Impressoras**. Não use `printerTypes` no `config.json` local.

## Painel com serviço instalado

Com o serviço **GestorEscolar** em execução, abra o painel em:

**http://127.0.0.1:17345**

Ou execute o atalho do agente — ele detecta o serviço e abre o navegador nessa URL.
