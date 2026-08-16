#!/usr/bin/env bash
# Instala o agente de impressão (Linux arm64). Requer root.
# Uso: sudo ./install.sh

set -euo pipefail

INSTALL_DIR="/opt/gestor-escolar"
SERVICE_NAME="gestor-escolar"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN="${SCRIPT_DIR}/gestor-escolar"

if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
  echo "Execute como root: sudo $0" >&2
  exit 1
fi

if [[ ! -f "${BIN}" ]]; then
  echo "Binário gestor-escolar não encontrado em ${SCRIPT_DIR}" >&2
  exit 1
fi

install -d -m 0755 "${INSTALL_DIR}"
install -m 0755 "${BIN}" "${INSTALL_DIR}/gestor-escolar"

if [[ ! -f "${INSTALL_DIR}/config.json" ]]; then
  if [[ -f "${SCRIPT_DIR}/config.json.example" ]]; then
    install -m 0640 "${SCRIPT_DIR}/config.json.example" "${INSTALL_DIR}/config.json"
    echo "Criado ${INSTALL_DIR}/config.json — edite server, name e enrollmentKey antes de iniciar."
  else
    echo "Aviso: crie ${INSTALL_DIR}/config.json manualmente." >&2
  fi
fi

install -m 0644 "${SCRIPT_DIR}/gestor-escolar.service" "/etc/systemd/system/${SERVICE_NAME}.service"
systemctl daemon-reload
systemctl enable "${SERVICE_NAME}.service"

if [[ -f "${INSTALL_DIR}/config.json" ]]; then
  systemctl restart "${SERVICE_NAME}.service" || systemctl start "${SERVICE_NAME}.service"
  echo "Serviço ${SERVICE_NAME} instalado e iniciado."
else
  echo "Serviço ${SERVICE_NAME} instalado. Configure config.json e execute: systemctl start ${SERVICE_NAME}"
fi
