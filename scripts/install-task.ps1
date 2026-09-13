<#
install-task.ps1 - register (or remove) the warden as a Windows scheduled task
that starts at logon.

    ./install-task.ps1            # register + start
    ./install-task.ps1 -Remove    # unregister
    ./install-task.ps1 -WhatIf    # show what would be registered

The warden must outlive its consumers: it is what STARTS the LLM backend they
talk to, so it cannot live inside any one consumer's lifecycle. It is also the
thing that decides when a game gets the card, and a policy that only exists while
Episteme's containers are up is a policy with a hole in it.

Runs as the logged-in user, not SYSTEM: it reads HKCU (Windows' Game Bar
catalogue) and per-process GPU counters for the user's own session, and needs no
elevation for any of it. That is also what puts its tray icon in the user's own
notification area rather than in session 0, where nobody would see it.

It starts hidden with a tray icon; double-clicking the icon shows its console,
which carries both llama-server logs and its own. See console.py.

Upgrading from Episteme's host agent: remove the old task first, or two processes
race for :5003.
    ./install-task.ps1 -Remove -TaskName EpistemeLlamaAgent
#>
[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [switch]$Remove,
    [string]$TaskName = "LlamaWarden"
)

$RepoRoot = Split-Path -Parent $PSScriptRoot
$Project  = Join-Path $RepoRoot "pyproject.toml"
$Runner   = Join-Path $PSScriptRoot "run-agent.ps1"
$LogFile  = "C:\selfhosting\llama-cpp\logs\agent.log"

if ($Remove) {
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction Stop
    Write-Host "Removed scheduled task '$TaskName'." -ForegroundColor Cyan
    return
}

$uv = (Get-Command uv -ErrorAction SilentlyContinue).Source
if (-not $uv) { throw "uv not found on PATH; the warden is run with 'uv run'." }
if (-not (Test-Path $Project)) { throw "Project not found at $Project" }
if (-not (Test-Path $Runner))  { throw "Runner not found at $Runner" }

New-Item -ItemType Directory -Path (Split-Path $LogFile) -Force | Out-Null

# Launched THROUGH conhost.exe, and that is the whole reason this line looks odd.
# On Windows 11 the default terminal application is Windows Terminal, and the
# console handoff leaves `GetConsoleWindow()` pointing at a pseudo-console window
# that the visible UI does not live in - so the hide-to-tray would move nothing
# and the close-button removal would apply to a window nobody can click.
# `conhost.exe <command>` opts out of the handoff and gives a classic console,
# which is what those win32 calls were designed against.
#
# `--headless` and `-Spawn` together are what make the logon flashless. Task
# Scheduler always starts its action shown and offers no way to pass SW_HIDE, so
# a task action that IS the console can only hide itself after the fact, which
# was measured at 419 ms of visible window. A headless conhost has no window at
# all, and the `-Spawn` stage inside it creates the real console with
# -WindowStyle Hidden, so that one is never shown even once.
#
# No output redirection any more: the process owns this console (it draws a text
# UI in it) and writes its own rotating `agent.log`. A `*>` here would both blank
# the UI and make stdout a file, which is exactly how it decides it has no
# console to draw in.
$action = New-ScheduledTaskAction -Execute "conhost.exe" `
    -Argument "--headless pwsh.exe -NoProfile -ExecutionPolicy Bypass -File `"$Runner`" -Spawn"

$trigger = New-ScheduledTaskTrigger -AtLogOn -User "$env:USERDOMAIN\$env:USERNAME"

# ExecutionTimeLimit 0 = never kill it; this is a long-running service, not a job.
# RestartCount/Interval bring it back if it crashes, and that matters more here
# than it did before the split: a warden that dies while a consumer is paused
# leaves it paused, because a pause is a message and not a lease (0001).
$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -DontStopOnIdleEnd `
    -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)

if ($PSCmdlet.ShouldProcess($TaskName, "Register scheduled task")) {
    Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger `
        -Settings $settings -Description "llama-warden: llama.cpp lifecycle and GPU contention policy (loopback :5003)" `
        -Force | Out-Null
    Start-ScheduledTask -TaskName $TaskName
    Write-Host "Registered and started '$TaskName'." -ForegroundColor Cyan
    Write-Host "  tray icon: llama-warden (double-click shows the console)"
    Write-Host "  log:       $LogFile"
    Write-Host "  verify:    curl http://127.0.0.1:5003/verdict"
}
