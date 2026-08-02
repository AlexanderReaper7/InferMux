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
no elevation for any of it.
#>
[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [switch]$Remove,
    [string]$TaskName = "EpistemeLlamaAgent"
)

$RepoRoot = Split-Path -Parent $PSScriptRoot
$AgentPy  = Join-Path $PSScriptRoot "llama_agent.py"
$LogFile  = "C:\selfhosting\llama-cpp\logs\agent.log"

if ($Remove) {
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction Stop
    Write-Host "Removed scheduled task '$TaskName'." -ForegroundColor Cyan
    return
}

$uv = (Get-Command uv -ErrorAction SilentlyContinue).Source
if (-not $uv) { throw "uv not found on PATH; the agent is run with 'uv run'." }
if (-not (Test-Path $AgentPy)) { throw "Agent not found at $AgentPy" }

New-Item -ItemType Directory -Path (Split-Path $LogFile) -Force | Out-Null

# `uv run` resolves the agent's PEP-723 dependency header, so there is no venv to
# create or keep in sync. Output is truncated on start (`*>`), matching the
# launcher's log policy - this is a debugging aid, not an archive.
$command = "& '$uv' run --directory '$RepoRoot' '$AgentPy' *> '$LogFile'"
$action = New-ScheduledTaskAction -Execute "pwsh.exe" `
    -Argument "-NoProfile -NonInteractive -WindowStyle Hidden -Command `"$command`""

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
    Write-Host "  agent log: $LogFile"
    Write-Host "  verify:    curl http://127.0.0.1:5003/status"
}
