; Per-user installer for the doremote connector: installs to %LOCALAPPDATA%\Programs\doremote
; without admin rights, so the connector can update itself by running a newer setup silently
; (connector/update_windows.go). Built by packaging/windows/build.sh with Inno Setup 6:
;   ISCC.exe /DAppVersion=1.2.3 packaging\windows\doremote.iss
; Expects dist\doremote-windows-amd64.exe and dist\doremote-windows-arm64.exe; writes
; dist\doremote-setup.exe.

#ifndef AppVersion
  #define AppVersion "0.0.0"
#endif

[Setup]
AppId={{E1548D29-630B-4CEB-AC8D-D400B07A11AD}
AppName=doremote
AppVersion={#AppVersion}
AppVerName=doremote {#AppVersion}
AppPublisher=doremote
AppPublisherURL=https://doremote.dev
VersionInfoVersion={#AppVersion}
PrivilegesRequired=lowest
DefaultDirName={localappdata}\Programs\doremote
DisableDirPage=yes
DisableProgramGroupPage=yes
DisableReadyPage=yes
ArchitecturesAllowed=x64compatible or arm64
ArchitecturesInstallIn64BitMode=x64compatible or arm64
CloseApplications=no
RestartApplications=no
SetupIconFile=doremote.ico
UninstallDisplayIcon={app}\doremote.exe
UninstallDisplayName=doremote
WizardStyle=modern
Compression=lzma2
SolidCompression=yes
OutputDir=..\..\dist
OutputBaseFilename=doremote-setup

[Tasks]
Name: autostart; Description: "Start doremote when I sign in"

[Files]
Source: "..\..\dist\doremote-windows-amd64.exe"; DestDir: "{app}"; DestName: "doremote.exe"; Check: not IsArm64; Flags: ignoreversion
Source: "..\..\dist\doremote-windows-arm64.exe"; DestDir: "{app}"; DestName: "doremote.exe"; Check: IsArm64; Flags: ignoreversion
Source: "stop-doremote.ps1"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{autoprograms}\doremote"; Filename: "{app}\doremote.exe"

[Registry]
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: string; ValueName: "doremote"; ValueData: """{app}\doremote.exe"""; Flags: uninsdeletevalue; Tasks: autostart

[Run]
Filename: "{app}\doremote.exe"; Description: "Start doremote"; Flags: nowait postinstall skipifsilent

[UninstallRun]
Filename: "powershell.exe"; Parameters: "-NoProfile -NonInteractive -ExecutionPolicy Bypass -File ""{app}\stop-doremote.ps1"""; Flags: runhidden; RunOnceId: "StopConnector"

[Code]
// Stops running connectors before files are replaced. A connector that is updating itself
// started this setup silently and quits on its own, so silent runs give it time to do so.
function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  Code: Integer;
  Wait: String;
begin
  Result := '';
  ExtractTemporaryFile('stop-doremote.ps1');
  if WizardSilent then
    Wait := '30'
  else
    Wait := '0';
  // ExecAndLogOutput copies the script's output into the setup log.
  if not ExecAndLogOutput('powershell.exe', '-NoProfile -NonInteractive -ExecutionPolicy Bypass -File "' +
      ExpandConstant('{tmp}\stop-doremote.ps1') + '" -WaitSeconds ' + Wait,
      '', SW_HIDE, ewWaitUntilTerminated, Code, nil) then
    Log('stop-doremote.ps1 could not be started: ' + SysErrorMessage(Code))
  else if Code <> 0 then
    Log('stop-doremote.ps1 exited with ' + IntToStr(Code));
end;

// Silent runs are updates started by the connector, which has quit by now: start whichever
// copy is installed afterwards, the new one or, if setup failed, the previous one.
procedure DeinitializeSetup();
var
  Exe: String;
  Code: Integer;
begin
  if not WizardSilent then
    Exit;
  try
    Exe := ExpandConstant('{app}\doremote.exe');
  except
    Exit; // setup ended before the install location was known
  end;
  if FileExists(Exe) then
    Exec(Exe, '-updated', '', SW_SHOWNORMAL, ewNoWait, Code);
end;
