<#
install-task.ps1 - register (or remove) the llama.cpp control agent as a
Windows scheduled task that starts at logon.

The agent must outlive Episteme: it is what STARTS Episteme's LLM backend, so it
cannot live inside the compose lifecycle it exists to serve. Architecture §12
specified exactly this ("host-side idle agent installed separately - tiny Python
script + Task Scheduler").

    ./install-task.ps1            # register + start
    ./install-task.ps1 -Remove    # unregister
    ./install-task.ps1 -WhatIf    # show what would be registered

Runs as the logged-in user, not SYSTEM: the agent reads HKCU (Windows' Game Bar
catalogue) and per-process GPU counters for the user's own session, and it needs
no elevation for any of it. That is also what puts its tray icon in the user's
own notification area rather than in session 0, where nobody would see it.

The agent starts hidden with a tray icon; double-clicking the icon shows its
console, which carries both llama-server logs and its own. See console.py.
#>
[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [switch]$Remove,
    [string]$TaskName = "EpistemeLlamaAgent"
)

$AgentPy  = Join-Path $PSScriptRoot "llama_agent.py"
$Runner   = Join-Path $PSScriptRoot "run-agent.ps1"
$LogFile  = "C:\selfhosting\llama-cpp\logs\agent.log"

if ($Remove) {
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction Stop
    Write-Host "Removed scheduled task '$TaskName'." -ForegroundColor Cyan
    return
}

$uv = (Get-Command uv -ErrorAction SilentlyContinue).Source
if (-not $uv) { throw "uv not found on PATH; the agent is run with 'uv run'." }
if (-not (Test-Path $AgentPy)) { throw "Agent not found at $AgentPy" }
if (-not (Test-Path $Runner))  { throw "Runner not found at $Runner" }

New-Item -ItemType Directory -Path (Split-Path $LogFile) -Force | Out-Null

# Launched THROUGH conhost.exe, and that is the whole reason this line looks odd.
# On Windows 11 the default terminal application is Windows Terminal, and the
# console handoff leaves `GetConsoleWindow()` pointing at a pseudo-console window
# that the visible UI does not live in - so the agent's hide-to-tray would move
# nothing and its close-button removal would apply to a window nobody can click.
# `conhost.exe <command>` opts out of the handoff and gives a classic console,
# which is what those win32 calls were designed against.
#
# `--headless` and `-Spawn` together are what make the logon flashless. Task
# Scheduler always starts its action shown and offers no way to pass SW_HIDE, so
# a task action that IS the agent's console can only hide itself after the fact,
# which was measured at 419 ms of visible window. A headless conhost has no
# window at all, and the `-Spawn` stage inside it creates the agent's real
# console with -WindowStyle Hidden, so that one is never shown even once.
#
# No output redirection any more: the agent owns this console (it draws a text UI
# in it) and writes its own rotating `agent.log`. A `*>` here would both blank the
# UI and make stdout a file, which is exactly how the agent decides it has no
# console to draw in.
$action = New-ScheduledTaskAction -Execute "conhost.exe" `
    -Argument "--headless pwsh.exe -NoProfile -ExecutionPolicy Bypass -File `"$Runner`" -Spawn"

$trigger = New-ScheduledTaskTrigger -AtLogOn -User "$env:USERDOMAIN\$env:USERNAME"

# ExecutionTimeLimit 0 = never kill it; this is a long-running service, not a job.
# RestartCount/Interval bring it back if it crashes. It binds loopback only, so
# there is nothing to gate on network availability.
$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -DontStopOnIdleEnd `
    -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)

if ($PSCmdlet.ShouldProcess($TaskName, "Register scheduled task")) {
    Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger `
        -Settings $settings -Description "Episteme llama.cpp host control agent (loopback :5003)" `
        -Force | Out-Null
    Start-ScheduledTask -TaskName $TaskName
    Write-Host "Registered and started '$TaskName'." -ForegroundColor Cyan
    Write-Host "  tray icon: llama.cpp agent (double-click shows the console)"
    Write-Host "  agent log: $LogFile"
    Write-Host "  verify:    curl http://127.0.0.1:5003/status"
}
