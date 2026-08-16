#!/usr/bin/env bash
# Build + execução local do agente (dev).
set -euo pipefail

# Resolve a partir da própria localização do script: o caminho absoluto anterior
# (~/DESENVOLVIMENTO/gestor-escolar/print-agent) só funcionava na máquina de quem escreveu.
cd "$(dirname "$0")"

go build -ldflags "-s -w -X main.agentVersion=$(cat VERSION)" -o ./gestor-escolar .
./gestor-escolar
