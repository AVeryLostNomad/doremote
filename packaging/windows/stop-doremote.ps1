# Stops this Windows session's doremote connectors so setup can replace or remove doremote.exe.
# Used by the installer (before copying files) and the uninstaller; its output goes to their log.
param([int]$WaitSeconds = 0)
$ErrorActionPreference = 'Stop'

$session = (Get-Process -Id $PID).SessionId
function Get-Connectors {
  , @(Get-CimInstance Win32_Process -Filter "Name = 'doremote.exe'" | Where-Object { $_.SessionId -eq $session })
}
function Wait-Connectors([int]$Seconds) {
  $deadline = (Get-Date).AddSeconds($Seconds)
  while ((Get-Connectors).Count -gt 0 -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 250 }
}

$all = Get-Connectors
"doremote processes in session ${session}: $($all.Count)"
if ($all.Count -eq 0) { exit 0 }

# A connector that is updating itself quits right after starting setup.
if ($WaitSeconds -gt 0) {
  "waiting up to $WaitSeconds s for them to quit"
  Wait-Connectors $WaitSeconds
}

# Stop the connectors but not their display guards (child doremote.exe processes): a guard notices
# its connector is gone and switches the screens a session turned off back on, then exits.
$all = Get-Connectors
$ids = @($all | ForEach-Object { $_.ProcessId })
foreach ($p in $all | Where-Object { $ids -notcontains $_.ParentProcessId }) {
  "stopping connector $($p.ProcessId)"
  Stop-Process -Id $p.ProcessId -Force -ErrorAction SilentlyContinue
}
$left = Get-Connectors # assigned first: the function returns its array as one object
foreach ($p in $left) {
  "stopping $($p.ProcessId), still running"
  Stop-Process -Id $p.ProcessId -Force -ErrorAction SilentlyContinue
}
"doremote processes left: $((Get-Connectors).Count)"
