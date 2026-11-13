; Inno Setup 6 script for the Status Page Windows installer.
;
; Build (from the repository root):
;   GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o dist/statuspage.exe ./cmd/statuspage
;   iscc /DAppVersion=1.2.3 installer\statuspage.iss
; Result: dist\StatusPageSetup-1.2.3.exe
;
; Optional defines: /DAppVersion=... (default 0.0.0-dev),
;                   /DSourceExe=... (default ..\dist\statuspage.exe, relative to this file)
;
; What it does:
;   * installs statuspage.exe into "C:\Program Files\Status Page";
;   * data directory C:\ProgramData\StatusPage (only SYSTEM and Administrators);
;   * fresh install: asks for the HTTP port and the database (SQLite or
;     PostgreSQL, the PostgreSQL connection is tested before continuing), runs
;     "statuspage setup" to create the tables and config.json, installs and
;     starts the "Status Page" Windows service and optionally opens the port in
;     the Windows Firewall;
;   * upgrade (config.json exists): only replaces the program and restarts the
;     service; database migrations run automatically on start;
;   * uninstall: removes the service and the firewall rule, keeps the data.

#if Ver < EncodeVer(6, 3, 0)
  #error Inno Setup 6.3 or newer is required
#endif

#ifndef AppVersion
  #define AppVersion "0.0.0-dev"
#endif
#ifndef SourceExe
  #define SourceExe "..\dist\statuspage.exe"
#endif

#define AppName "Status Page"
#define ServiceName "StatusPage"
#define FirewallRule "Status Page"

[Setup]
AppId={{022C9EC9-B9B9-47C5-82D3-14B99EA41A54}
AppName={#AppName}
AppVersion={#AppVersion}
AppVerName={#AppName} {#AppVersion}
AppPublisher={#AppName}
AppPublisherURL=https://github.com/szilvasidave/status-page-tool
AppSupportURL=https://github.com/szilvasidave/status-page-tool
DefaultDirName={autopf}\{#AppName}
DisableProgramGroupPage=yes
OutputDir=..\dist
OutputBaseFilename=StatusPageSetup-{#AppVersion}
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
PrivilegesRequired=admin
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0
UninstallDisplayName={#AppName}
UninstallDisplayIcon={app}\statuspage.exe
SetupLogging=yes

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "firewall"; Description: "Allow incoming connections to the status page port in the Windows Firewall"

[Files]
Source: "{#SourceExe}"; DestDir: "{app}"; DestName: "statuspage.exe"; Flags: ignoreversion

[Dirs]
Name: "{commonappdata}\StatusPage"; Flags: uninsneveruninstall

[Run]
Filename: "{code:GetBaseURL}/setup"; Description: "Open the status page setup in the browser"; Flags: postinstall shellexec nowait skipifsilent; Check: IsFreshInstall
Filename: "{code:GetBaseURL}/"; Description: "Open the status page in the browser"; Flags: postinstall shellexec nowait skipifsilent unchecked; Check: IsUpgrade

[Code]
var
  PortPage: TInputQueryWizardPage;
  DbPage: TInputOptionWizardPage;
  PgPage: TInputQueryWizardPage;
  ProgressPage: TOutputProgressWizardPage;
  Upgrade: Boolean;

{ Field indexes on the PostgreSQL page (two columns, row by row). }
const
  PgHost = 0;
  PgPort = 1;
  PgDatabase = 2;
  PgSSLMode = 3;
  PgUser = 4;
  PgPassword = 5;

function DataDir(): String;
begin
  Result := ExpandConstant('{commonappdata}\StatusPage');
end;

function ConfigFile(): String;
begin
  Result := DataDir() + '\config.json';
end;

function IsUpgrade(): Boolean;
begin
  Result := Upgrade;
end;

function IsFreshInstall(): Boolean;
begin
  Result := not Upgrade;
end;

function UsePostgres(): Boolean;
begin
  Result := DbPage.SelectedValueIndex = 1;
end;

function ValidPort(const S: String): Boolean;
var
  Port: Integer;
begin
  Port := StrToIntDef(Trim(S), 0);
  Result := (Port >= 1) and (Port <= 65535);
end;

{ ---------------------------------------------------------------- config.json }

{ Returns the value of a top level string setting in config.json (the file is
  written by statuspage.exe with one "key": "value" pair per line). }
function ConfigValue(const Key: String): String;
var
  S: AnsiString;
  Text: String;
  P: Integer;
begin
  Result := '';
  if not LoadStringFromFile(ConfigFile(), S) then
    Exit;
  Text := String(S);
  P := Pos('"' + Key + '"', Text);
  if P = 0 then
    Exit;
  Text := Copy(Text, P + Length(Key) + 2, Length(Text));
  P := Pos('"', Text);
  if P = 0 then
    Exit;
  Text := Copy(Text, P + 1, Length(Text));
  P := Pos('"', Text);
  if P = 0 then
    Exit;
  Result := Copy(Text, 1, P - 1);
end;

{ The HTTP port: entered on the port page, or read from config.json when
  upgrading (listen is e.g. ":8080" or "0.0.0.0:8080"). }
function GetPort(): String;
var
  Listen: String;
  P: Integer;
begin
  if not Upgrade then
  begin
    Result := IntToStr(StrToIntDef(Trim(PortPage.Values[0]), 8080));
    Exit;
  end;
  Listen := ConfigValue('listen');
  P := Pos(':', Listen);
  while P > 0 do
  begin
    Listen := Copy(Listen, P + 1, Length(Listen));
    P := Pos(':', Listen);
  end;
  if ValidPort(Listen) then
    Result := Listen
  else
    Result := '8080';
end;

function GetBaseURL(Param: String): String;
begin
  if Upgrade and (ConfigValue('tls_cert') <> '') then
    Result := 'https://localhost:' + GetPort()
  else
    Result := 'http://localhost:' + GetPort();
end;

{ ---------------------------------------------------------------- statuspage.exe setup }

{ Returns S as a JSON string literal. Non-ASCII characters are written as
  \uXXXX escapes (UTF-16 code units, as JSON expects) so that the file is
  plain ASCII. }
function JsonString(const S: String): String;
var
  I, C: Integer;
begin
  Result := '"';
  for I := 1 to Length(S) do
  begin
    C := Ord(S[I]);
    if C = 34 then
      Result := Result + '\"'
    else if C = 92 then
      Result := Result + '\\'
    else if (C < 32) or (C > 126) then
      Result := Result + Format('\u%.4x', [C])
    else
      Result := Result + S[I];
  end;
  Result := Result + '"';
end;

{ The settings for "statuspage setup -params-file". }
function ParamsJson(): String;
begin
  Result := '{"listen":' + JsonString(':' + GetPort());
  if UsePostgres() then
    Result := Result + ',"driver":"postgres","postgres":{' +
      '"host":' + JsonString(Trim(PgPage.Values[PgHost])) +
      ',"port":' + IntToStr(StrToIntDef(Trim(PgPage.Values[PgPort]), 5432)) +
      ',"database":' + JsonString(Trim(PgPage.Values[PgDatabase])) +
      ',"user":' + JsonString(Trim(PgPage.Values[PgUser])) +
      ',"password":' + JsonString(PgPage.Values[PgPassword]) +
      ',"sslmode":' + JsonString(Trim(PgPage.Values[PgSSLMode])) + '}'
  else
    Result := Result + ',"driver":"sqlite"';
  Result := Result + '}';
end;

{ Runs "<Exe> setup" with the entered settings. Returns True on success,
  otherwise False and the error message in ErrorMsg. }
function RunSetup(const Exe: String; const CheckOnly: Boolean; var ErrorMsg: String): Boolean;
var
  ParamsFile, ResultFile, Args: String;
  Lines: TArrayOfString;
  ResultCode, I: Integer;
begin
  Result := False;
  ErrorMsg := '';
  ParamsFile := ExpandConstant('{tmp}\statuspage-params.json');
  ResultFile := ExpandConstant('{tmp}\statuspage-result.txt');
  DeleteFile(ResultFile);
  if not SaveStringToFile(ParamsFile, ParamsJson(), False) then
  begin
    ErrorMsg := 'Could not write the temporary file ' + ParamsFile + '.';
    Exit;
  end;
  Args := 'setup -data ' + AddQuotes(DataDir()) + ' -params-file ' + AddQuotes(ParamsFile) +
    ' -result-file ' + AddQuotes(ResultFile);
  if CheckOnly then
    Args := Args + ' -check-only';
  Log('Running: ' + Exe + ' ' + Args);
  if not Exec(Exe, Args, '', SW_HIDE, ewWaitUntilTerminated, ResultCode) then
    ErrorMsg := 'Could not run ' + Exe + ': ' + SysErrorMessage(ResultCode)
  else if LoadStringsFromFile(ResultFile, Lines) and (GetArrayLength(Lines) > 0) then
  begin
    for I := 0 to GetArrayLength(Lines) - 1 do
    begin
      if I > 0 then
        ErrorMsg := ErrorMsg + #13#10;
      ErrorMsg := ErrorMsg + Lines[I];
    end;
    Result := (ResultCode = 0) and (Trim(ErrorMsg) = 'OK');
  end
  else
    ErrorMsg := Format('The database setup failed (exit code %d).', [ResultCode]);
  { The parameters contain the database password. }
  DeleteFile(ParamsFile);
  Log('Setup result: ' + ErrorMsg);
end;

{ Runs a program hidden and waits for it; returns its exit code (or -1 if it
  could not be started). }
function RunProgram(const Exe, Args: String): Integer;
var
  ResultCode: Integer;
begin
  Log('Running: ' + Exe + ' ' + Args);
  if Exec(Exe, Args, '', SW_HIDE, ewWaitUntilTerminated, ResultCode) then
  begin
    Log(Format('Exit code: %d', [ResultCode]));
    Result := ResultCode;
  end
  else
  begin
    Log('Could not start the program: ' + SysErrorMessage(ResultCode));
    Result := -1;
  end;
end;

{ Runs statuspage.exe from the installation directory. }
function RunStatusPage(const Args: String): Integer;
begin
  Result := RunProgram(ExpandConstant('{app}\statuspage.exe'), Args);
end;

{ Runs a tool from the Windows system directory, e.g. netsh.exe. }
function RunSystemTool(const Tool, Args: String): Integer;
begin
  Result := RunProgram(ExpandConstant('{sys}\') + Tool, Args);
end;

{ ---------------------------------------------------------------- wizard }

{ Arranges the six fields of the PostgreSQL page in two columns so that they
  fit on the page. }
procedure ArrangePgPage();
var
  I, ColWidth, Gap, RowHeight, EditOffset, Top0: Integer;
begin
  Gap := ScaleX(16);
  ColWidth := (PgPage.SurfaceWidth - Gap) div 2;
  Top0 := PgPage.PromptLabels[0].Top;
  EditOffset := PgPage.Edits[0].Top - PgPage.PromptLabels[0].Top;
  RowHeight := PgPage.PromptLabels[1].Top - PgPage.PromptLabels[0].Top;
  for I := 0 to 5 do
  begin
    { Fixed size, so that the columns never overlap when the window grows. }
    PgPage.PromptLabels[I].Anchors := [akLeft, akTop];
    PgPage.Edits[I].Anchors := [akLeft, akTop];
    PgPage.PromptLabels[I].Left := (I mod 2) * (ColWidth + Gap);
    PgPage.PromptLabels[I].Top := Top0 + (I div 2) * RowHeight;
    PgPage.PromptLabels[I].Width := ColWidth;
    PgPage.Edits[I].Left := PgPage.PromptLabels[I].Left;
    PgPage.Edits[I].Top := PgPage.PromptLabels[I].Top + EditOffset;
    PgPage.Edits[I].Width := ColWidth;
  end;
end;

function InitializeSetup(): Boolean;
begin
  Upgrade := FileExists(ConfigFile());
  if Upgrade then
    Log('Existing configuration found, upgrading: ' + ConfigFile());
  Result := True;
end;

procedure InitializeWizard();
begin
  PortPage := CreateInputQueryPage(wpSelectDir,
    'Web server port', 'On which TCP port should the status page listen?',
    'Users open the status page in their browser at http://<this server>:<port>/. ' +
    'Choose a port that is not used by another program. You can put a reverse proxy ' +
    'with HTTPS in front of it later.');
  PortPage.Add('&Port:', False);
  PortPage.Values[0] := '8080';

  DbPage := CreateInputOptionPage(PortPage.ID,
    'Database', 'Where should the status page store its data?',
    'SQLite needs no further setup. For PostgreSQL the database must already exist; ' +
    'the tables are created automatically.',
    True, False);
  DbPage.Add('SQLite (built in, recommended for small installations)');
  DbPage.Add('PostgreSQL');
  DbPage.SelectedValueIndex := 0;

  PgPage := CreateInputQueryPage(DbPage.ID,
    'PostgreSQL connection', 'How does the status page connect to PostgreSQL?',
    'SSL mode: disable, allow, prefer, require, verify-ca or verify-full. ' +
    'The connection is tested when you click Next.');
  PgPage.Add('&Server name:', False);
  PgPage.Add('P&ort:', False);
  PgPage.Add('&Database:', False);
  PgPage.Add('SSL &mode:', False);
  PgPage.Add('&User:', False);
  PgPage.Add('Pass&word:', True);
  PgPage.Values[PgHost] := 'localhost';
  PgPage.Values[PgPort] := '5432';
  PgPage.Values[PgDatabase] := 'statuspage';
  PgPage.Values[PgSSLMode] := 'prefer';
  PgPage.Values[PgUser] := 'statuspage';
  ArrangePgPage();

  ProgressPage := CreateOutputProgressPage('PostgreSQL connection', 'Testing the connection to PostgreSQL, please wait...');
end;

function ShouldSkipPage(PageID: Integer): Boolean;
begin
  Result := False;
  if Upgrade and ((PageID = PortPage.ID) or (PageID = DbPage.ID) or (PageID = PgPage.ID)) then
    Result := True
  else if (PageID = PgPage.ID) and not UsePostgres() then
    Result := True;
end;

function NextButtonClick(CurPageID: Integer): Boolean;
var
  ErrorMsg: String;
begin
  Result := True;
  if CurPageID = PortPage.ID then
  begin
    if not ValidPort(PortPage.Values[0]) then
    begin
      SuppressibleMsgBox('Please enter a port number between 1 and 65535.', mbError, MB_OK, IDOK);
      Result := False;
    end;
  end
  else if CurPageID = PgPage.ID then
  begin
    if not ValidPort(PgPage.Values[PgPort]) then
    begin
      SuppressibleMsgBox('Please enter a PostgreSQL port number between 1 and 65535.', mbError, MB_OK, IDOK);
      Result := False;
      Exit;
    end;
    if not FileExists(ExpandConstant('{tmp}\statuspage.exe')) then
      ExtractTemporaryFile('statuspage.exe');
    ProgressPage.SetText('Connecting to ' + Trim(PgPage.Values[PgHost]) + '...', '');
    ProgressPage.Show;
    try
      Result := RunSetup(ExpandConstant('{tmp}\statuspage.exe'), True, ErrorMsg);
    finally
      ProgressPage.Hide;
    end;
    if not Result then
      SuppressibleMsgBox('The PostgreSQL settings do not work:' + #13#10#13#10 + ErrorMsg, mbError, MB_OK, IDOK);
  end;
end;

{ ---------------------------------------------------------------- install }

function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  { Stop the running service so that statuspage.exe can be replaced.
    "net stop" waits until the service has stopped; it fails harmlessly if
    the service does not exist or is not running. }
  RunSystemTool('net.exe', 'stop {#ServiceName}');
  Result := '';
end;

procedure SetupFirewall();
begin
  RunSystemTool('netsh.exe', 'advfirewall firewall delete rule name="{#FirewallRule}"');
  if RunSystemTool('netsh.exe', 'advfirewall firewall add rule name="{#FirewallRule}" dir=in action=allow protocol=TCP localport=' + GetPort()) <> 0 then
    SuppressibleMsgBox('The Windows Firewall rule for port ' + GetPort() + ' could not be created. ' +
      'Please allow the port manually.', mbError, MB_OK, IDOK);
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  ErrorMsg: String;
begin
  if CurStep <> ssPostInstall then
    Exit;

  { Only SYSTEM (the service) and Administrators may access the data
    directory: it contains the secret key and the database credentials.
    (statuspage.exe restricts it the same way when it starts.) }
  RunSystemTool('icacls.exe', AddQuotes(DataDir()) +
    ' /inheritance:r /grant:r *S-1-5-18:(OI)(CI)F *S-1-5-32-544:(OI)(CI)F /Q');

  if not Upgrade then
  begin
    WizardForm.StatusLabel.Caption := 'Creating the database tables...';
    if not RunSetup(ExpandConstant('{app}\statuspage.exe'), False, ErrorMsg) then
    begin
      { The service starts in setup mode; this file makes it listen on the
        chosen port (instead of 8080) until the setup is completed, and the
        setup (in the browser or "statuspage setup") keeps that port. }
      SaveStringToFile(DataDir() + '\setup-listen.txt', ':' + GetPort(), False);
      SuppressibleMsgBox('The database could not be set up:' + #13#10#13#10 + ErrorMsg + #13#10#13#10 +
        'You can complete the database setup in the browser at ' + GetBaseURL('') + '/setup ' +
        'once the installation has finished.', mbError, MB_OK, IDOK);
    end;
  end;

  WizardForm.StatusLabel.Caption := 'Installing the Windows service...';
  { Also points an existing "StatusPage" service (e.g. registered by a
    portable statuspage.exe) to this program and data directory. }
  if RunStatusPage('service install -data ' + AddQuotes(DataDir())) <> 0 then
    SuppressibleMsgBox('The Windows service could not be installed. See the setup log for details.', mbError, MB_OK, IDOK);

  if WizardIsTaskSelected('firewall') then
  begin
    WizardForm.StatusLabel.Caption := 'Configuring the Windows Firewall...';
    SetupFirewall();
  end;

  { "service start" waits (up to 15 seconds) until the status page answers
    and fails if it does not, e.g. because the port is used by another
    program. }
  WizardForm.StatusLabel.Caption := 'Starting the Windows service...';
  if RunStatusPage('service start -data ' + AddQuotes(DataDir())) <> 0 then
    SuppressibleMsgBox('The Windows service "{#AppName}" did not start or does not answer at ' + GetBaseURL('') + '. ' +
      'Please check the log file ' + DataDir() + '\statuspage.log' + #13#10#13#10 +
      'A common cause is a port that is already used by another program: change "listen" in ' +
      ConfigFile() + ' and restart the service.', mbError, MB_OK, IDOK);
end;

{ ---------------------------------------------------------------- uninstall }

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  ResultCode: Integer;
begin
  if CurUninstallStep = usUninstall then
  begin
    { Stops and removes the service before the files are deleted. }
    Exec(ExpandConstant('{app}\statuspage.exe'), 'service uninstall -data ' + AddQuotes(DataDir()),
      '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
    Exec(ExpandConstant('{sys}\netsh.exe'), 'advfirewall firewall delete rule name="{#FirewallRule}"',
      '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
  end
  else if (CurUninstallStep = usPostUninstall) and DirExists(DataDir()) and not UninstallSilent() then
    MsgBox('The data directory ' + DataDir() + ' (configuration, SQLite database and log files) ' +
      'was kept. Delete it manually if you no longer need it.', mbInformation, MB_OK);
end;
