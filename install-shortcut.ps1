<#
install-shortcut.ps1 - put an "Episteme host agent" shortcut in the Start menu
(and optionally on the desktop) that runs open-agent.ps1.

    ./install-shortcut.ps1              # Start menu only, searchable by name
    ./install-shortcut.ps1 -Desktop     # ...and on the desktop
    ./install-shortcut.ps1 -Remove      # take both away

Start menu by default and the desktop only on request: the Start menu entry costs
nothing and makes the agent findable by typing its name, where a desktop icon is
somebody else's wallpaper.

The shortcut wears graphics/logo/hostagent.ico, which `tools/build_hostagent_ico.py`
renders from the same `icon_image` the tray uses, so the taskbar button and the
notification-area icon are one picture rather than two that resemble each other.

Launched through `conhost.exe --headless` for the same reason install-task.ps1 is:
a plain `pwsh -WindowStyle Hidden` target still creates a window and hides it,
which is a visible flash at every launch, and on Windows 11 the Terminal handoff
would also leave the agent's own win32 calls pointing at a window its UI does not
live in. A headless console host has no window at all, and open-agent.ps1 shows
the agent's real one once it exists.
#>
[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [switch]$Desktop,
    [switch]$Remove,
    [string]$Name = "Episteme host agent"
)

$ErrorActionPreference = "Stop"

$Opener = Join-Path $PSScriptRoot "open-agent.ps1"
$Icon = Join-Path (Split-Path -Parent $PSScriptRoot) "graphics\logo\hostagent.ico"
$StartMenu = Join-Path ([Environment]::GetFolderPath("Programs")) "$Name.lnk"
$DesktopLnk = Join-Path ([Environment]::GetFolderPath("Desktop")) "$Name.lnk"

if ($Remove) {
    foreach ($path in @($StartMenu, $DesktopLnk)) {
        if (Test-Path $path) {
            Remove-Item $path -Force
            Write-Host "Removed $path" -ForegroundColor Cyan
        }
    }
    return
}

if (-not (Test-Path $Opener)) { throw "Opener not found at $Opener" }
if (-not (Test-Path $Icon)) {
    throw "Icon not found at $Icon - run 'uv run tools/build_hostagent_ico.py' first."
}
$pwsh = (Get-Command pwsh -ErrorAction SilentlyContinue).Source
if (-not $pwsh) { throw "pwsh not found on PATH." }

$targets = @($StartMenu)
if ($Desktop) { $targets += $DesktopLnk }

$shell = New-Object -ComObject WScript.Shell
foreach ($path in $targets) {
    if (-not $PSCmdlet.ShouldProcess($path, "Create shortcut")) { continue }
    $lnk = $shell.CreateShortcut($path)
    $lnk.TargetPath = "conhost.exe"
    $lnk.Arguments = "--headless `"$pwsh`" -NoProfile -ExecutionPolicy Bypass -File `"$Opener`" -Quiet"
    $lnk.WorkingDirectory = $PSScriptRoot
    $lnk.IconLocation = "$Icon,0"
    $lnk.Description = "Start the Episteme llama.cpp host agent, or show the running one."
    $lnk.Save()
    Write-Host "Wrote $path" -ForegroundColor Cyan
}
