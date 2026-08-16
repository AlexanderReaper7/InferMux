<#
open-agent.ps1 - launch the host agent, or bring the running one to the front.

What the desktop/Start-menu shortcut runs. One entry point for both states,
because from the outside they are the same intention ("show me the agent") and a
person should not have to know which one they are in.

    ./open-agent.ps1              # show it, starting it first if it is not up
    ./open-agent.ps1 -Quiet       # same, without the console chatter

The running instance is found by its window TITLE, which `console.py:run` sets to
'llama.cpp agent' before it hides anything. That is an exact match on a string the
agent owns, so this needs no new HTTP route to poke and no guessing at which of
uv's, Python's or conhost's processes holds the window - and `FindWindowW` finds
hidden windows, which is the only kind this one is when it is in the tray.

Starting it goes through run-agent.ps1 -Spawn, the same path the scheduled task
uses, so the agent still owns a classic console created hidden and hide-to-tray
still works. This script then waits for that window and shows it: the task wants
it invisible at logon, a person double-clicking a shortcut wants to see it, and
the difference belongs here rather than in a second way of starting the agent.
#>
[CmdletBinding()]
param(
    [switch]$Quiet,

    # How long to wait for the window after a cold start. uv resolves the
    # environment first, so the first run after a lockfile change is much slower
    # than the steady-state one.
    [int]$TimeoutSeconds = 60
)

$ErrorActionPreference = "Stop"
$Title = "llama.cpp agent"
# Matched on the class as well as the title. `ConsoleWindowClass` is what
# `conhost.exe <command>` produces - the classic console the agent's own win32
# calls are written against - so this cannot latch onto an editor window that
# happens to have the agent's name in its caption.
$Class = "ConsoleWindowClass"
$Runner = Join-Path $PSScriptRoot "run-agent.ps1"

function Write-Status([string]$Message) {
    if (-not $Quiet) { Write-Host $Message -ForegroundColor Cyan }
}

$signature = @'
[DllImport("user32.dll", CharSet = CharSet.Unicode)] public static extern IntPtr FindWindowW(string lpClassName, string lpWindowName);
[DllImport("user32.dll")] public static extern bool ShowWindow(IntPtr hWnd, int nCmdShow);
[DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr hWnd);
'@
$Win = Add-Type -MemberDefinition $signature -Name Windows -Namespace OpenAgent -PassThru

function Get-AgentWindow {
    # NOT $null for a string parameter. PowerShell's binder converts $null to the
    # empty string there, and FindWindowW then looks for a window class literally
    # named "", which never matches - so the running agent reads as absent and the
    # shortcut starts a second one. Cost an otherwise clean 60-second timeout
    # against a window that was sitting right there (2026-08-15).
    $Win::FindWindowW($Class, $Title)
}

function Show-AgentWindow([IntPtr]$Handle) {
    # SW_RESTORE, which un-hides as well as un-minimises, so one call covers both
    # "in the tray" and "minimised".
    $null = $Win::ShowWindow($Handle, 9)
    # Best effort. Windows refuses SetForegroundWindow to a process that is not
    # itself in the foreground, so run by hand from another terminal this leaves
    # the window visible but unfocused (measured 2026-08-15). Launched from the
    # shortcut the shell grants the foreground right and it takes focus. Not worth
    # the AttachThreadInput dance to close that gap: the window is up either way.
    $null = $Win::SetForegroundWindow($Handle)
}

$hwnd = Get-AgentWindow
if ($hwnd -ne [IntPtr]::Zero) {
    Show-AgentWindow $hwnd
    Write-Status "Agent already running; brought its console to the front."
    return
}

# No window, but the port may still be bound: `--headless` is a supported way to
# run the agent and it has no console to show. Saying so beats starting a second
# instance that would fail to bind :5003 and die somewhere the user cannot see.
$bound = @(Get-NetTCPConnection -LocalPort 5003 -State Listen -ErrorAction SilentlyContinue)
if ($bound.Count -gt 0) {
    Write-Warning "Agent is listening on :5003 but has no console window (started --headless). Nothing to show."
    return
}

if (-not (Test-Path $Runner)) { throw "Runner not found at $Runner" }
Write-Status "Starting the agent..."
& $Runner -Spawn

$deadline = (Get-Date).AddSeconds($TimeoutSeconds)
while ((Get-Date) -lt $deadline) {
    Start-Sleep -Milliseconds 250
    $hwnd = Get-AgentWindow
    if ($hwnd -ne [IntPtr]::Zero) {
        Show-AgentWindow $hwnd
        Write-Status "Agent started; console is up and the tray icon is live."
        return
    }
}

Write-Warning "Started the agent but its window did not appear within $TimeoutSeconds s. Check C:\selfhosting\llama-cpp\logs\agent.log."
