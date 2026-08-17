#!/usr/bin/env bash
# Build completo + instaladores Linux (.tar.gz) e Windows (.exe via Inno Setup)
# Uso: ./build.sh [--help]
# Saídas em dist/

set -euo pipefail

cd "$(dirname "$0")"

VERSION="$(tr -d '[:space:]' < VERSION)"
DIST="dist"
APP="gestor-escolar"
LDFLAGS="-s -w -X main.agentVersion=${VERSION}"
WIN_LDFLAGS="${LDFLAGS} -H windowsgui"

usage() {
  cat <<EOF
Uso: ./build.sh

Build completo (sem argumentos):
  - dist/gestor-escolar-linux-arm64-{VERSION}.tar.gz  (instalador Linux ARM64)
  - dist/gestor-escolar.exe                           (binário Windows x86)
  - dist/GestorEscolar-Setup-{VERSION}.exe            (instalador Windows)

Requer Docker (instalador Windows via amake/innosetup), ou iscc/Wine local.

Assinatura Authenticode (opcional — sem estas variáveis o build gera .exe não assinado):
  CODESIGN_CMD            comando que recebe o .exe como último argumento e assina in-place
                          (ex.: signtool, AzureSignTool, jsign, osslsigncode com engine PKCS#11)
  CODESIGN_PFX            caminho de um .pfx/.p12 (só certificados legados — desde jun/2023 a
                          baseline CA/B exige a chave em token/HSM, use CODESIGN_CMD nesse caso)
  CODESIGN_PFX_PASS       senha do .pfx
  CODESIGN_TIMESTAMP_URL  servidor RFC3161 (default: http://timestamp.digicert.com)
  CODESIGN_ISCC_SIGNTOOL  comando de assinatura para o próprio Inno Setup, no formato
                          '<cmd com \$f>'. Só assim o desinstalador (unins000.exe) sai assinado;
                          exige ISCC nativo/Wine (não funciona no caminho Docker).

Versão: ${VERSION} (arquivo VERSION)
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" || "${1:-}" == "help" ]]; then
  usage
  exit 0
fi

if [[ -n "${1:-}" ]]; then
  echo "Este script não aceita targets. Execute apenas: ./build.sh" >&2
  usage >&2
  exit 1
fi

# Assinatura Authenticode. É no-op quando nenhuma variável CODESIGN_* está definida — o build segue
# e produz os mesmos artefatos não assinados de sempre, para não quebrar quem builda sem certificado.
#
# Por que isto existe: Smart App Control (Win11 22H2+, instalação limpa) bloqueia executável sem
# assinatura e **não oferece bypass por arquivo** — o usuário só consegue instalar desligando o SAC,
# o que é irreversível sem reinstalar o Windows. O mesmo vale para o .exe que `update.go` baixa no
# auto-update. Ver README.md § Assinatura de código.
codesign_enabled() {
  [[ -n "${CODESIGN_CMD:-}" || -n "${CODESIGN_PFX:-}" ]]
}

sign_file() {
  local file="$1"
  local ts="${CODESIGN_TIMESTAMP_URL:-http://timestamp.digicert.com}"

  if ! codesign_enabled; then
    return 0
  fi

  if [[ ! -f "${file}" ]]; then
    echo "ERRO: assinatura pedida mas arquivo não existe: ${file}" >&2
    exit 1
  fi

  echo "Assinando ${file}..."
  if [[ -n "${CODESIGN_CMD:-}" ]]; then
    # Word splitting proposital: CODESIGN_CMD é um comando com flags fornecido pelo operador.
    # shellcheck disable=SC2086
    ${CODESIGN_CMD} "${file}"
  else
    if ! command -v osslsigncode &>/dev/null; then
      echo "ERRO: CODESIGN_PFX definido mas osslsigncode não encontrado no PATH." >&2
      echo "  Instale (apt install osslsigncode) ou use CODESIGN_CMD." >&2
      exit 1
    fi
    local tmp="${file}.signed"
    osslsigncode sign \
      -pkcs12 "${CODESIGN_PFX}" \
      -pass "${CODESIGN_PFX_PASS:-}" \
      -n "Gestor Escolar" \
      -i "https://app.pedagogicoonline.com.br" \
      -h sha256 \
      -ts "${ts}" \
      -in "${file}" \
      -out "${tmp}"
    mv "${tmp}" "${file}"
  fi

  # Sem verificação, um comando de assinatura que sai 0 mas não grava nada (token travado, perfil
  # errado) produziria artefato "assinado" que o Windows recusa — falha que só apareceria na escola.
  if command -v osslsigncode &>/dev/null; then
    if ! osslsigncode verify -in "${file}" >/dev/null 2>&1; then
      echo "ERRO: ${file} continua sem assinatura válida após a assinatura." >&2
      exit 1
    fi
    echo "OK: assinatura verificada em ${file}"
  else
    echo "AVISO: osslsigncode ausente — assinatura de ${file} não verificada." >&2
  fi
}

build_linux() {
  local arch="$1"
  local out="$2"
  echo "Building Linux ${arch} v${VERSION}..."
  GOOS=linux GOARCH="${arch}" go build -ldflags "${LDFLAGS}" -o "${out}" .
  echo "OK: ${out}"
}

build_windows_386() {
  local out="${DIST}/${APP}.exe"
  echo "Building Windows 386 v${VERSION}..."
  GOOS=windows GOARCH=386 go build -ldflags "${WIN_LDFLAGS}" -o "${out}" .
  echo "OK: ${out}"
}

package_linux() {
  local goarch="$1"   # arm64
  local label="$2"    # arm64
  local linux_dir="${DIST}/${APP}-linux-${label}-${VERSION}"

  mkdir -p "${linux_dir}"
  build_linux "${goarch}" "${linux_dir}/${APP}"

  install -m 0644 config.json.example "${linux_dir}/config.json.example"
  install -m 0644 installer/linux/gestor-escolar.service "${linux_dir}/gestor-escolar.service"
  install -m 0755 installer/linux/install.sh "${linux_dir}/install.sh"
  install -m 0755 installer/linux/uninstall.sh "${linux_dir}/uninstall.sh"

  local tarball="${DIST}/${APP}-linux-${label}-${VERSION}.tar.gz"
  tar -C "${DIST}" -czf "${tarball}" "$(basename "${linux_dir}")"
  echo "OK: ${tarball}"
}

compile_windows_installer() {
  local win_setup="${DIST}/GestorEscolar-Setup-${VERSION}.exe"
  local iss_file="installer/gestor-escolar.iss"
  local root
  root="$(pwd)"

  # SumatraPDF.exe não é versionado no git — é dependência de build de terceiro, obtida uma vez e
  # mantida só em disco. Sem este check, o Inno Setup falharia com "source file not found", que não
  # diz o que fazer; com ele, a mensagem aponta direto para o download.
  if [[ ! -f "${root}/SumatraPDF.exe" ]]; then
    echo "ERRO: ${root}/SumatraPDF.exe não encontrado — necessário para o instalador Windows." >&2
    echo "  Baixe a versão portátil em https://www.sumatrapdfreader.org/download-free-pdf-viewer" >&2
    echo "  e salve como ${root}/SumatraPDF.exe (ver print-agent/README.md § Pré-requisitos)." >&2
    exit 1
  fi

  run_iscc() {
    echo "Compilando instalador Windows..."
    (cd "${root}/installer" && "$@")
  }

  # O desinstalador (unins000.exe) é gerado pelo próprio Inno a partir de um stub embutido, então
  # não dá para assiná-lo por fora: só o ISCC consegue, via diretiva SignTool + SignedUninstaller.
  # Ativado apenas quando o operador define CODESIGN_ISCC_SIGNTOOL (ver usage).
  local -a iss_args=()
  if [[ -n "${CODESIGN_ISCC_SIGNTOOL:-}" ]]; then
    iss_args=(/DSignToolName=gesigner "/Sgesigner=${CODESIGN_ISCC_SIGNTOOL}")
  fi

  if [[ -n "${ISCC:-}" ]]; then
    (cd "${root}/installer" && eval "${ISCC} /DMyAppVersion=${VERSION} ${iss_args[*]@Q} gestor-escolar.iss")
  elif command -v iscc &>/dev/null; then
    run_iscc iscc /DMyAppVersion="${VERSION}" "${iss_args[@]}" gestor-escolar.iss
  else
    local wine_iscc found=false
    for wine_iscc in \
      "${HOME}/.wine/drive_c/Program Files (x86)/Inno Setup 6/ISCC.exe" \
      "${HOME}/.wine/drive_c/Program Files/Inno Setup 6/ISCC.exe"
    do
      if [[ -f "${wine_iscc}" ]]; then
        run_iscc wine "${wine_iscc}" /DMyAppVersion="${VERSION}" "${iss_args[@]}" gestor-escolar.iss
        found=true
        break
      fi
    done

    if [[ "${found}" != true ]]; then
      if ! command -v docker &>/dev/null; then
        echo "ERRO: instalador Windows não gerado: ${win_setup}" >&2
        echo "  Instale Docker, ou Inno Setup (iscc/Wine), ou defina ISCC." >&2
        exit 1
      fi
      # O container amake/innosetup não tem o token/HSM nem o binário de assinatura do host —
      # aceitar o pedido aqui produziria um instalador com desinstalador não assinado em silêncio.
      if [[ ${#iss_args[@]} -gt 0 ]]; then
        echo "ERRO: CODESIGN_ISCC_SIGNTOOL exige ISCC nativo ou Wine — o caminho Docker não assina." >&2
        echo "  Instale Inno Setup local / defina ISCC, ou remova CODESIGN_ISCC_SIGNTOOL." >&2
        exit 1
      fi
      echo "Compilando instalador Windows (Docker amake/innosetup)..."
      docker run --rm -i \
        -v "${root}:/work" \
        -w /work \
        amake/innosetup \
        /DMyAppVersion="${VERSION}" \
        "${iss_file}"
    fi
  fi

  if [[ ! -f "${win_setup}" ]]; then
    echo "ERRO: instalador Windows não gerado: ${win_setup}" >&2
    exit 1
  fi

  local win_app="${DIST}/${APP}.exe"
  if cmp -s "${win_setup}" "${win_app}"; then
    echo "ERRO: ${win_setup} é idêntico ao binário ${win_app} — Inno Setup não gerou o instalador." >&2
    echo "  Verifique Docker/iscc e rode novamente: ./build.sh" >&2
    exit 1
  fi

  # Quando CODESIGN_ISCC_SIGNTOOL está ativo o próprio Inno já assinou o setup; re-assinar aqui só
  # gastaria uma assinatura da cota (Trusted Signing cobra por assinatura) sem mudar o resultado.
  if [[ -z "${CODESIGN_ISCC_SIGNTOOL:-}" ]]; then
    sign_file "${win_setup}"
  fi

  echo "OK: ${win_setup}"
}

publish_frontend() {
  local pub_root="../frontend/public"
  local pub_dir="${pub_root}/print-agent"
  local win_exe_name="${APP}-${VERSION}.exe"
  local win_setup_name="GestorEscolar-Setup-${VERSION}.exe"
  local linux_tar_name="${APP}-linux-arm64-${VERSION}.tar.gz"
  local win_exe="${pub_dir}/${win_exe_name}"
  local win_setup="${pub_dir}/${win_setup_name}"
  local linux_tar="${pub_dir}/${linux_tar_name}"

  # O agente também vive em repositório próprio (ZoglioControllato/gestor-escolar-print-agent), onde
  # não existe `../frontend`. Lá o build para aqui: os artefatos ficam em dist/ e a publicação no app
  # web continua sendo passo do monorepo. Sem este guard, `install` falharia no fim de um build ok.
  if [[ ! -d "${pub_root}" ]]; then
    echo "Sem ${pub_root} — publicação no app web pulada (build fora do monorepo)."
    return 0
  fi

  echo "Publicando artefatos em ${pub_dir}..."
  mkdir -p "${pub_dir}"

  install -m 0644 "${DIST}/${APP}.exe" "${win_exe}"
  install -m 0644 "${DIST}/GestorEscolar-Setup-${VERSION}.exe" "${win_setup}"
  install -m 0644 "${DIST}/${linux_tar_name}" "${linux_tar}"

  local sha386 sha_arm64
  sha386=$(sha256sum "${win_exe}" | awk '{print $1}')
  sha_arm64=$(sha256sum "${linux_tar}" | awk '{print $1}')

  cat > "${pub_root}/print-agent-version.json" <<EOF
{
  "version": "${VERSION}",
  "artifacts": {
    "386": {
      "url": "/print-agent/${win_exe_name}",
      "sha256": "${sha386}"
    },
    "arm64": {
      "url": "/print-agent/${linux_tar_name}",
      "sha256": "${sha_arm64}"
    }
  },
  "setup": {
    "386": "/print-agent/${win_setup_name}"
  }
}
EOF

  echo "OK: ${pub_root}/print-agent-version.json"
}

echo "=== Gestor Escolar print-agent v${VERSION} ==="
rm -rf "${DIST}"
mkdir -p "${DIST}"

if codesign_enabled; then
  echo "Assinatura de código: ATIVA"
else
  echo "Assinatura de código: DESATIVADA — artefatos Windows sairão sem assinatura."
  echo "  Smart App Control bloqueia esses .exe sem opção de bypass. Ver README.md § Assinatura de código."
fi

package_linux arm64 arm64
build_windows_386
# Antes do Inno empacotar: o instalador carrega o binário dentro dele, então assinar depois deixaria
# o gestor-escolar.exe instalado (e o baixado pelo auto-update) sem assinatura.
sign_file "${DIST}/${APP}.exe"
compile_windows_installer
publish_frontend

echo ""
echo "=== Build concluído ==="
echo "  ${DIST}/${APP}-linux-arm64-${VERSION}.tar.gz"
echo "  ${DIST}/${APP}.exe"
echo "  ${DIST}/GestorEscolar-Setup-${VERSION}.exe"
echo "  ../frontend/public/print-agent/"
echo "  ../frontend/public/print-agent-version.json"
echo ""
echo "Próximo passo: npm run pack:print-agent --prefix ../frontend"
