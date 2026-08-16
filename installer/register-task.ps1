# Fallback: tarefa ao logon (redes que bloqueiam criação de serviço).
# Executar como administrador. Uso: .\register-task.ps1 (cwd = pasta do agente)

$exe = Join-Path $PSScriptRoot "gestor-escolar.exe"
if (-not (Test-Path $exe)) {
    Write-Error "gestor-escolar.exe nao encontrado em $PSScriptRoot"
    exit 1
}

$taskName = "GestorEscolarLogon"
schtasks /Delete /TN $taskName /F 2>$null
schtasks /Create /TN $taskName /TR "`"$exe`"" /SC ONLOGON /RL HIGHEST /F
if ($LASTEXITCODE -ne 0) {
    Write-Error "schtasks falhou com codigo $LASTEXITCODE"
    exit $LASTEXITCODE
}
Write-Host "Tarefa $taskName criada. Execute gestor-escolar.exe para iniciar."
