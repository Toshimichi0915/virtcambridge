# Run in the guest as admin, next to videobridge.exe (and optionally ffmpeg.exe):
#   .\install.ps1
# Runs at logon in your session (virtual cameras like OBS's only work there), elevated
# so it can open the IVSHMEM device. Shares every DirectShow virtual camera.
$ErrorActionPreference = "Stop"

$dir = "$env:ProgramFiles\videobridge"
New-Item -ItemType Directory -Force $dir | Out-Null
Get-ScheduledTask videobridge -ErrorAction SilentlyContinue | Stop-ScheduledTask
# Stop-ScheduledTask doesn't wait; make sure our exe and its ffmpegs have exited before copying.
Get-Process videobridge, ffmpeg -EA SilentlyContinue | ? { $_.Path -like "$dir\*" } |
    Stop-Process -Force -PassThru | Wait-Process -EA SilentlyContinue
Copy-Item "$PSScriptRoot\videobridge.exe" $dir -Force

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
Start-ScheduledTask videobridge
"Installed. Log: $env:TEMP\videobridge.log"
