#!/usr/bin/env bash
# Remove o agente de impressão (Linux). Requer root.
# Uso: sudo ./uninstall.sh

set -euo pipefail

INSTALL_DIR="/opt/gestor-escolar"
SERVICE_NAME="gestor-escolar"

if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
  echo "Execute como root: sudo $0" >&2
  exit 1
fi

systemctl stop "${SERVICE_NAME}.service" 2>/dev/null || true
systemctl disable "${SERVICE_NAME}.service" 2>/dev/null || true
rm -f "/etc/systemd/system/${SERVICE_NAME}.service"
systemctl daemon-reload

if [[ -d "${INSTALL_DIR}" ]]; then
  echo "Removendo ${INSTALL_DIR} (config.json e token.txt serão apagados)."
  rm -rf "${INSTALL_DIR}"
fi

echo "Desinstalação concluída."
