# Instaladores — Agente de Impressão

Um comando gera binários e instaladores em `dist/`.

## Pré-requisitos

| Ferramenta | Uso |
|------------|-----|
| Go 1.22+ | Cross-compile Linux + Windows |
| Docker | Instalador Windows (`amake/innosetup` — fallback automático) |
| Inno Setup 6 | Alternativa local (`iscc` ou Wine) |
| systemd + CUPS | Instalação Linux na máquina destino |

### Instalador Windows no Linux

Por padrão o `build.sh` usa Docker:

```bash
docker run --rm -i -v "$PWD:/work" -w /work amake/innosetup ...
```

Na primeira execução a imagem `amake/innosetup` é baixada (~500 MB).

Alternativas: `iscc` nativo (Windows), Wine + Inno Setup, ou `export ISCC='...'`.

## Build

```bash
cd print-agent
./build.sh
```

Saída em `dist/`:

| Artefato | Descrição |
|----------|-----------|
| `gestor-escolar-linux-arm64-{VERSION}.tar.gz` | Instalador Linux ARM64 |
| `gestor-escolar.exe` | Binário Windows x86 (386) |
| `GestorEscolar-Setup-{VERSION}.exe` | Instalador Windows |

Versão: arquivo `VERSION` (passada ao Inno via `/DMyAppVersion`).

## Instalação Linux

```bash
tar xzf dist/gestor-escolar-linux-arm64-*.tar.gz
cd gestor-escolar-linux-arm64-*
sudo ./install.sh
sudo nano /opt/gestor-escolar/config.json
sudo systemctl restart gestor-escolar
```

## Instalação Windows

Execute `dist/GestorEscolar-Setup-{VERSION}.exe` como administrador.

- **Serviço (padrão):** `GestorEscolar`
- **Tarefa ao logon:** `GestorEscolarLogon` (alternativa)

Configure o agente pelo **painel local** (http://127.0.0.1:17345) ou edite `%ProgramData%\GestorEscolar\config.json` (`server`, `name`, `enrollmentKey`). O `server` deve ser o **mesmo ambiente** do app web (prod ou devel).

Ver [`../TROUBLESHOOTING.md`](../TROUBLESHOOTING.md) se o dispositivo não aparecer no admin.

## Desinstalação

- Linux: `sudo ./uninstall.sh` (no pacote extraído)
- Windows: desinstalador do setup; tarefa agendada: `schtasks /Delete /TN GestorEscolarLogon /F`
