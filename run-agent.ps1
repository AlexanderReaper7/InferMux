<#
run-agent.ps1 - what the scheduled task actually executes.

Exists so the task's command line stays readable. Registering this logic inline
meant a PowerShell string, inside a Task Scheduler argument, containing a nested
quoted command - three levels of escaping for something nobody could then read
back out of `Get-ScheduledTask`.

It does one thing the agent cannot do for itself: keep the console off the screen
*immediately*. The agent hides its own window too (`--hide`), but only once uv has
resolved the environment and Python has started.

There are two ways to be off the screen and only one of them is any good. Hiding
after the fact (the `-Show`-less branch below) still shows the window first:
measured at 419 ms of console on a 4K desktop at every logon. Being *created*
hidden - `STARTF_USESHOWWINDOW` + `SW_HIDE`, which is what `Start-Process
-WindowStyle Hidden` sets - never shows it at all, measured as never observed
visible. That is what `-Spawn` is for, and why the scheduled task uses it.

Run by hand:
    ./run-agent.ps1          # what the task's second stage runs; hides itself
    ./run-agent.ps1 -Show    # leave the window up and watch startup
#>
[CmdletBinding()]
param(
    # Skip the hide, for when you are debugging startup and want to watch it.
    [switch]$Show,

    # Relaunch through a console created hidden, then exit. Task Scheduler has no
    # way to say SW_HIDE, so the task starts this stage inside `conhost --headless`
    # (a console host with no window of its own) and this stage makes the real one.
    [switch]$Spawn,

    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$PassthroughArgs
)

$RepoRoot = Split-Path -Parent $PSScriptRoot
$AgentPy = Join-Path $PSScriptRoot "llama_agent.py"

if ($Spawn) {
    # A window created hidden is one `-Show` could never show: SW_HIDE has already
    # happened by the time this script's child starts. Refuse rather than pick one.
    if ($Show) { throw "-Spawn and -Show are mutually exclusive; run without -Spawn to watch startup." }

    # conhost.exe, not pwsh.exe: on Windows 11 the default terminal is Windows
    # Terminal, and that handoff leaves GetConsoleWindow() pointing at a window
    # the visible UI does not live in, so hide-to-tray would move nothing. Same
    # reason install-task.ps1 goes through conhost; see the comment there.
    $inner = "pwsh.exe -NoProfile -ExecutionPolicy Bypass -File `"$PSCommandPath`""
    if ($PassthroughArgs) { $inner += " " + ($PassthroughArgs -join " ") }
    Start-Process conhost.exe -ArgumentList $inner -WindowStyle Hidden
    return
}

# --hide tells the agent this console is its own, which is what licenses it to
# remove the close button AND to hide itself at startup. `-Show` has to withhold
# it, not merely skip the hide below: the agent would otherwise hide the window
# half a second after this script decided to leave it up, which is a debugging
# switch that does not debug.
#
# The ShowWindow below is a no-op when the parent used -Spawn, since the window
# was born hidden. It stays for the hand-run case, where this console is visible
# and would otherwise sit there until Python got far enough to hide it.
$agentArgs = @()

if (-not $Show) {
    $signature = @'
[DllImport("kernel32.dll")] public static extern IntPtr GetConsoleWindow();
[DllImport("user32.dll")] public static extern bool ShowWindow(IntPtr hWnd, int nCmdShow);
'@
    $win = Add-Type -MemberDefinition $signature -Name Console -Namespace Agent -PassThru
    $null = $win::ShowWindow($win::GetConsoleWindow(), 0)  # SW_HIDE
    $agentArgs += "--hide"
}

& uv run --directory $RepoRoot $AgentPy @agentArgs @PassthroughArgs
