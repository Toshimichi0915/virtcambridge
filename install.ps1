# Double-click install.cmd next to videobridge.exe (and optionally ffmpeg.exe), or:
#   powershell -ExecutionPolicy Bypass -File .\install.ps1 [-Uninstall]
# Runs at logon in your session (virtual cameras like OBS's only work there), elevated
# so it can open the IVSHMEM device. Shares every DirectShow virtual camera.
# Registers in Settings > Apps, whose Uninstall runs this script with -Uninstall.
param([switch]$Uninstall)
$ErrorActionPreference = "Stop"

# Self-elevate: install.cmd and Settings' Uninstall both start us unelevated.
# -NoExit on install keeps the window open so the result (or error) stays readable.
if (-not ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole("Administrators")) {
    $a = "-NoProfile -ExecutionPolicy Bypass -File `"$PSCommandPath`""
    if ($Uninstall) { $a += " -Uninstall" } else { $a = "-NoExit $a" }
    Start-Process powershell -Verb RunAs -ArgumentList $a
    exit
}

$dir = "$env:ProgramFiles\videobridge"
$key = "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\videobridge"
Get-ScheduledTask videobridge -ErrorAction SilentlyContinue | Stop-ScheduledTask
# Stop-ScheduledTask doesn't wait; make sure our exe and its ffmpegs have exited before copying.
Get-Process videobridge, ffmpeg -EA SilentlyContinue | ? { $_.Path -like "$dir\*" } |
    Stop-Process -Force -PassThru | Wait-Process -EA SilentlyContinue

if ($Uninstall) {
    Unregister-ScheduledTask videobridge -Confirm:$false -EA SilentlyContinue
    Remove-Item $dir -Recurse -Force -EA SilentlyContinue
    Remove-Item $key -Force -EA SilentlyContinue
    return
}

New-Item -ItemType Directory -Force $dir | Out-Null
Copy-Item "$PSScriptRoot\videobridge.exe", $PSCommandPath $dir -Force

# ffmpeg goes next to videobridge.exe so the task doesn't depend on PATH.
# Order: beside this script, already on PATH, else install it with winget.
$ff = "$PSScriptRoot\ffmpeg.exe"
if (-not (Test-Path $ff)) { $ff = (Get-Command ffmpeg.exe -EA SilentlyContinue).Source }
if (-not $ff) {
    winget install --id Gyan.FFmpeg -e --accept-source-agreements --accept-package-agreements | Out-Host
    $ff = Get-ChildItem "$env:LOCALAPPDATA\Microsoft\WinGet\Packages\Gyan.FFmpeg*\*\bin\ffmpeg.exe" -EA SilentlyContinue |
        Select-Object -First 1 -ExpandProperty FullName
}
if (-not $ff) { throw "ffmpeg.exe not found; put it next to install.ps1 and run again" }
if ((Resolve-Path $ff).Path -ne "$dir\ffmpeg.exe") { Copy-Item $ff $dir -Force }

$task = New-ScheduledTask `
    -Action (New-ScheduledTaskAction -Execute "$dir\videobridge.exe") `
    -Trigger (New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME) `
    -Principal (New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType Interactive -RunLevel Highest) `
    -Settings (New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -AllowStartIfOnBatteries `
        -DontStopIfGoingOnBatteries -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1))
Register-ScheduledTask videobridge -InputObject $task -Force | Out-Null

# Settings > Apps entry. The script lives in Program Files (admin-only), so Bypass is safe.
New-Item $key -Force | Out-Null
@{
    DisplayName     = "videobridge"
    DisplayIcon     = "$dir\videobridge.exe"
    InstallLocation = $dir
    UninstallString = "powershell.exe -NoProfile -ExecutionPolicy Bypass -File `"$dir\install.ps1`" -Uninstall"
    NoModify        = 1
    NoRepair        = 1
}.GetEnumerator() | % { Set-ItemProperty $key $_.Key $_.Value }

Start-ScheduledTask videobridge
"Installed. Log: $env:TEMP\videobridge.log"
