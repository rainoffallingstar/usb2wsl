param(
  [Parameter(Mandatory=$true)][string]$ExePath,
  [Parameter(Mandatory=$true)][string]$ConfigPath,
  [string]$TaskName = "usb2wsl",
  [ValidateSet("ONLOGON","ONSTART")][string]$Trigger = "ONLOGON",
  [int]$DelaySeconds = 15,
  [int]$RestartCount = 3,
  [int]$RestartIntervalSeconds = 10,
  [int]$ExecutionTimeLimitMinutes = 0,
  [string]$WorkingDirectory = "",
  [string]$LogPath = ""
)

$exe = (Resolve-Path $ExePath).Path
$cfg = (Resolve-Path $ConfigPath).Path

if ($WorkingDirectory -eq "") {
  $WorkingDirectory = Split-Path -Parent $exe
}
if ($LogPath -ne "") {
  $logDir = Split-Path -Parent $LogPath
  if ($logDir -ne "" -and !(Test-Path $logDir)) {
    New-Item -ItemType Directory -Path $logDir | Out-Null
  }
}

$args = @("-config", "`"$cfg`"")
if ($LogPath -ne "") {
  $args += @("-log", "`"$LogPath`"")
}
$action = New-ScheduledTaskAction -Execute $exe -Argument ($args -join " ") -WorkingDirectory $WorkingDirectory

if ($Trigger -eq "ONSTART") {
  $triggerObj = New-ScheduledTaskTrigger -AtStartup
} else {
  $triggerObj = New-ScheduledTaskTrigger -AtLogOn
}

if ($DelaySeconds -gt 0) {
  $triggerObj.Delay = "PT${DelaySeconds}S"
}

$currentUser = "$env:USERDOMAIN\$env:USERNAME"
if ($Trigger -eq "ONSTART") {
  $principal = New-ScheduledTaskPrincipal -UserId "NT AUTHORITY\\SYSTEM" -LogonType ServiceAccount -RunLevel Highest
} else {
  $principal = New-ScheduledTaskPrincipal -UserId $currentUser -LogonType InteractiveToken -RunLevel Highest
}
$settings = New-ScheduledTaskSettingsSet
$settings.RestartCount = $RestartCount
$settings.RestartInterval = "PT${RestartIntervalSeconds}S"
if ($ExecutionTimeLimitMinutes -le 0) {
  $settings.ExecutionTimeLimit = "PT0S"
} else {
  $settings.ExecutionTimeLimit = "PT${ExecutionTimeLimitMinutes}M"
}

Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $triggerObj -Principal $principal -Settings $settings -Force | Out-Null

Write-Host "Created scheduled task: $TaskName"
Write-Host "Run manually: schtasks /Run /TN $TaskName"

