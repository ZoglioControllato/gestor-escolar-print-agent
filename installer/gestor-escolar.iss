; Inno Setup — build: cd .. && ./build.sh release (gera dist/gestor-escolar.exe)
; Versão sincronizada com ../VERSION

#define MyAppName "Gestor Escolar"
#ifndef MyAppVersion
  #define MyAppVersion "0.0.0"
#endif
#define MyAppPublisher "Gestor Escolar"

[Setup]
AppId={{B5F3E8A1-4C2D-4E9F-9A1B-2D3E4F5A6B7C}
AppName={#MyAppName}
AppVersion={#MyAppVersion}
AppPublisher={#MyAppPublisher}
DefaultDirName={sd}\GestorEscolar
DefaultGroupName={#MyAppName}
OutputDir=..\dist
OutputBaseFilename=GestorEscolar-Setup-{#MyAppVersion}
Compression=lzma2
SolidCompression=yes
PrivilegesRequired=admin
ArchitecturesAllowed=x86compatible
UninstallDisplayIcon={app}\gestor-escolar.exe

; Assinatura Authenticode do setup e do desinstalador. Só ativa quando build.sh recebe
; CODESIGN_ISCC_SIGNTOOL e passa /DSignToolName=<nome> /S<nome>=<comando>; sem isso o bloco some no
; pré-processador e a compilação segue igual à de antes (sem assinatura).
; unins000.exe é gerado a partir de um stub embutido no setup — SignedUninstaller é o único jeito de
; assiná-lo, já que ele não existe como arquivo em tempo de build.
#ifdef SignToolName
SignTool={#SignToolName}
SignedUninstaller=yes
#endif

[Tasks]
Name: "installservice"; Description: "Instalar como serviço Windows (GestorEscolar)"; GroupDescription: "Modo de execução:"; Flags: checkedonce exclusive
Name: "installtask"; Description: "Alternativa: tarefa agendada ao logon (redes restritas)"; GroupDescription: "Modo de execução:"; Flags: unchecked exclusive
Name: "startupshortcut"; Description: "Iniciar automaticamente com o Windows (atalho Startup do usuário)"; GroupDescription: "Modo de execução:"; Flags: unchecked exclusive

[Dirs]
; PPE-22 (diagnostic.md §3 achado #7): `users-full` no diretório do executável era a escalada de
; privilégio local — gestor-escolar.exe roda como SYSTEM no modo serviço (o padrão marcado abaixo em
; [Tasks]), e "Users" é o grupo embutido de TODO usuário local autenticado, não só quem instalou.
; Qualquer um deles podia sobrescrever o .exe e esperar o próximo restart do serviço para rodar como
; SYSTEM. Sem essa concessão ampla, só SYSTEM e Administradores (`*S-1-5-32-544`, SID bem-conhecido,
; independente de idioma do Windows) podem escrever no diretório — o mesmo par que
; `secure_windows.go` usa via `icacls` para token.txt/config.json em runtime.
;
; Trade-off aceito e não corrigido nesta task: os modos alternativos "tarefa agendada"/"atalho de
; Startup" (`installtask`/`startupshortcut`, ambos secundários — o serviço é o `checkedonce` acima)
; rodam como o usuário interativo, que precisaria escrever config.json/token.txt no mesmo diretório
; do .exe (`configDataDir()` = pasta do executável no Windows). Mover a config para um local gravável
; pelo usuário nesses dois modos é uma correção maior, fora do escopo desta task de segurança.
Name: "{app}"; Permissions: admins-full system-full
Name: "{app}\temp"; Permissions: users-modify system-full

[Files]
Source: "..\dist\gestor-escolar.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\SumatraPDF.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\SumatraPDF-settings.txt"; DestDir: "{app}"; Flags: skipifsourcedoesntexist
Source: "..\config.json.example"; DestDir: "{app}"; Flags: ignoreversion
Source: "register-task.ps1"; DestDir: "{app}"

[Icons]
Name: "{group}\Painel de Impressão"; Filename: "{app}\gestor-escolar.exe"; Parameters: "--open-panel"
Name: "{group}\Desinstalar {#MyAppName}"; Filename: "{uninstallexe}"
Name: "{userstartup}\{#MyAppName}"; Filename: "{app}\gestor-escolar.exe"; Tasks: startupshortcut

[Run]
Filename: "{sys}\sc.exe"; Parameters: "create GestorEscolar binPath= ""\""{app}\gestor-escolar.exe\"""" start= auto DisplayName= ""{#MyAppName}"""; Flags: runhidden; Tasks: installservice
Filename: "{sys}\sc.exe"; Parameters: "description GestorEscolar ""Agente de impressão Gestor Escolar"""; Flags: runhidden; Tasks: installservice
Filename: "{sys}\sc.exe"; Parameters: "start GestorEscolar"; Flags: runhidden; Tasks: installservice

Filename: "{app}\gestor-escolar.exe"; Parameters: "--open-panel"; Description: "Abrir painel de configuração"; Flags: postinstall nowait skipifsilent

[UninstallRun]
Filename: "{sys}\sc.exe"; Parameters: "stop GestorEscolar"; Flags: runhidden; RunOnceId: "StopService"
Filename: "{sys}\sc.exe"; Parameters: "delete GestorEscolar"; Flags: runhidden; RunOnceId: "DeleteService"

[Code]
var
  ConfigPage: TWizardPage;
  ServerEdit, KeyEdit, NameEdit: TEdit;

procedure InitializeWizard;
var
  LblServer, LblKey, LblName: TLabel;
begin
  ConfigPage := CreateCustomPage(wpSelectTasks, 'Vinculação à conta',
    'Informe a URL da API (mesmo ambiente do app web) e a chave de enrollment.' +
    Chr(13) + Chr(10) + 'Você pode alterar depois em http://127.0.0.1:17345');

  LblServer := TLabel.Create(ConfigPage);
  LblServer.Parent := ConfigPage.Surface;
  LblServer.Caption := 'URL da API (server):';
  LblServer.Top := 16;

  ServerEdit := TEdit.Create(ConfigPage);
  ServerEdit.Parent := ConfigPage.Surface;
  ServerEdit.Top := LblServer.Top + LblServer.Height + 4;
  ServerEdit.Width := ConfigPage.SurfaceWidth;
  ServerEdit.Text := 'https://api.pedagogicoonline.com.br';

  LblKey := TLabel.Create(ConfigPage);
  LblKey.Parent := ConfigPage.Surface;
  LblKey.Caption := 'Chave de enrollment:';
  LblKey.Top := ServerEdit.Top + ServerEdit.Height + 12;

  KeyEdit := TEdit.Create(ConfigPage);
  KeyEdit.Parent := ConfigPage.Surface;
  KeyEdit.Top := LblKey.Top + LblKey.Height + 4;
  KeyEdit.Width := ConfigPage.SurfaceWidth;

  LblName := TLabel.Create(ConfigPage);
  LblName.Parent := ConfigPage.Surface;
  LblName.Caption := 'Nome do agente (ex.: Sala Direção):';
  LblName.Top := KeyEdit.Top + KeyEdit.Height + 12;

  NameEdit := TEdit.Create(ConfigPage);
  NameEdit.Parent := ConfigPage.Surface;
  NameEdit.Top := LblName.Top + LblName.Height + 4;
  NameEdit.Width := ConfigPage.SurfaceWidth;
  NameEdit.Text := 'Agente';
end;

function EscapeJson(S: String): String;
var
  I: Integer;
  C: Char;
begin
  Result := '';
  for I := 1 to Length(S) do
  begin
    C := S[I];
    if C = '\' then
      Result := Result + '\\'
    else if C = '"' then
      Result := Result + '\"'
    else
      Result := Result + C;
  end;
end;

procedure WriteInitialConfig;
var
  ConfigDir, ConfigPath, Json: String;
begin
  ConfigDir := ExpandConstant('{app}');
  ConfigPath := ConfigDir + '\config.json';
  if FileExists(ConfigPath) then
    Exit;
  ForceDirectories(ConfigDir);
  Json :=
    '{' + Chr(13) + Chr(10) +
    '  "server": "' + EscapeJson(Trim(ServerEdit.Text)) + '",' + Chr(13) + Chr(10) +
    '  "appUrl": "https://app.pedagogicoonline.com.br",' + Chr(13) + Chr(10) +
    '  "name": "' + EscapeJson(Trim(NameEdit.Text)) + '",' + Chr(13) + Chr(10) +
    '  "tokenFile": "token.txt",' + Chr(13) + Chr(10) +
    '  "enrollmentKey": "' + EscapeJson(Trim(KeyEdit.Text)) + '",' + Chr(13) + Chr(10) +
    '  "sumatraPdfPath": "SumatraPDF.exe",' + Chr(13) + Chr(10) +
    '  "printSettings": "noscale"' + Chr(13) + Chr(10) +
    '}';
  SaveStringToFile(ConfigPath, Json, False);
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  ResultCode: Integer;
begin
  if CurStep = ssPostInstall then
  begin
    WriteInitialConfig;
    if WizardIsTaskSelected('installtask') then
    begin
      if not Exec('powershell.exe', '-NoProfile -ExecutionPolicy Bypass -File "' + ExpandConstant('{app}\register-task.ps1') + '"', ExpandConstant('{app}'), SW_HIDE, ewWaitUntilTerminated, ResultCode) then
        MsgBox('Não foi possível registrar a tarefa agendada. Execute register-task.ps1 manualmente como administrador.', mbInformation, MB_OK);
    end;
  end;
end;
