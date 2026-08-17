# Instaladores — Agente de Impressão

Um comando gera binários e instaladores em `dist/`.

## Pré-requisitos

| Ferramenta | Uso |
|------------|-----|
| Go 1.22+ | Cross-compile Linux + Windows |
| Docker | Instalador Windows (`amake/innosetup` — fallback automático) |
| Inno Setup 6 | Alternativa local (`iscc` ou Wine) |

## Build

```bash
./build.sh
```

## Instalação Linux

```bash
tar xzf dist/gestor-escolar-linux-arm64-*.tar.gz
cd gestor-escolar-linux-arm64-*
sudo ./install.sh
sudo nano /opt/gestor-escolar/config.json
sudo systemctl restart gestor-escolar
```

## Instalação Windows

Execute `dist/GestorEscolar-Setup-{VERSION}.exe` como administrador. Configure pelo painel local
(http://127.0.0.1:17345) ou editando `%ProgramData%\GestorEscolar\config.json`.

## Desinstalação

- Linux: `sudo ./uninstall.sh` (no pacote extraído)
- Windows: desinstalador do setup
