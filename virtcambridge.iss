; Guest installer: make setup -> virtcambridge-setup.exe
; Runs virtcambridge at logon in the user's session (virtual cameras like OBS's only work
; there), elevated so it can open the IVSHMEM device.
; ffmpeg: bundled if ffmpeg.exe sits next to this file at build time, else taken from PATH
; or installed with winget during setup.

#ifndef AppVersion
  #define AppVersion "dev"
#endif
#define Bundled FileExists(AddBackslash(SourcePath) + "ffmpeg.exe")

[Setup]
AppId=virtcambridge
AppName=virtcambridge
AppVersion={#AppVersion}
DefaultDirName={commonpf64}\virtcambridge
DisableDirPage=yes
DisableProgramGroupPage=yes
PrivilegesRequired=admin
ArchitecturesAllowed=x64compatible
LicenseFile=LICENSE
UninstallDisplayIcon={app}\virtcambridge.exe
OutputDir=.
OutputBaseFilename=virtcambridge-setup
WizardStyle=modern

[Files]
Source: "virtcambridge.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "LICENSE"; DestDir: "{app}"
#if Bundled
Source: "ffmpeg.exe"; DestDir: "{app}"; Flags: ignoreversion
#else
Source: "{tmp}\ffmpeg.exe"; DestDir: "{app}"; Flags: external ignoreversion
#endif

; Leftovers from the old install.ps1 installer.
[InstallDelete]
Type: files; Name: "{app}\install.ps1"

[Registry]
Root: HKLM64; Subkey: "SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\virtcambridge"; ValueType: none; Flags: deletekey

[Code]
// Runs a PowerShell snippet hidden; the snippet must not contain double quotes.
function PS(Cmd: String): Boolean;
var
  RC: Integer;
begin
  Result := Exec(ExpandConstant('{sysnative}\WindowsPowerShell\v1.0\powershell.exe'), '-NoProfile -ExecutionPolicy Bypass -Command "$ErrorActionPreference=''Stop''; ' + Cmd + '"',
    '', SW_HIDE, ewWaitUntilTerminated, RC) and (RC = 0);
end;

// Stop-ScheduledTask doesn't wait; make sure our exe and its ffmpegs have exited.
procedure StopRunning;
begin
  PS('Get-ScheduledTask virtcambridge -EA 0 | Stop-ScheduledTask; ' +
     'Get-Process virtcambridge, ffmpeg -EA 0 | ? { $_.Path -like ''' + ExpandConstant('{app}') + '\*'' } | ' +
     'Stop-Process -Force -PassThru | Wait-Process -EA 0');
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  StopRunning;
#if !Bundled
  WizardForm.PreparingLabel.Caption := 'Finding ffmpeg (installing it with winget if needed)...';
  if not PS('$ff = (Get-Command ffmpeg.exe -EA 0).Source; ' +
            'if (-not $ff) { winget install --id Gyan.FFmpeg -e --silent --accept-source-agreements --accept-package-agreements | Out-Null; ' +
            '$ff = Get-ChildItem $env:LOCALAPPDATA\Microsoft\WinGet\Packages\Gyan.FFmpeg*\*\bin\ffmpeg.exe -EA 0 | Select-Object -First 1 -ExpandProperty FullName }; ' +
            'Copy-Item $ff ''' + ExpandConstant('{tmp}') + '\ffmpeg.exe''') then
    Result := 'ffmpeg.exe not found and winget could not install it. Install ffmpeg (winget install Gyan.FFmpeg) and run setup again.';
#endif
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  App: String;
begin
  if CurStep <> ssPostInstall then Exit;
  App := ExpandConstant('{app}');
  if not PS('$t = New-ScheduledTask ' +
            '-Action (New-ScheduledTaskAction -Execute ''' + App + '\virtcambridge.exe'') ' +
            '-Trigger (New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME) ' +
            '-Principal (New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType Interactive -RunLevel Highest) ' +
            '-Settings (New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -AllowStartIfOnBatteries ' +
            '-DontStopIfGoingOnBatteries -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1)); ' +
            'Register-ScheduledTask virtcambridge -InputObject $t -Force | Out-Null; Start-ScheduledTask virtcambridge') then
    MsgBox('Files were installed, but registering the logon task failed. Run setup again.', mbError, MB_OK);
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep <> usUninstall then Exit;
  StopRunning;
  PS('Unregister-ScheduledTask virtcambridge -Confirm:$false -EA 0');
end;
