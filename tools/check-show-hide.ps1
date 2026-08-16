param([int]$Port = 5015)

# Spawns a throwaway agent on a spare port and proves the three claims about its
# window that nothing in pytest can reach:
#
#   1. it is born hidden, so a logon does not put a console on the desktop,
#   2. the tray's "show console" (ShowWindow SW_RESTORE) brings up a window that
#      has never been shown, which is not the same call path as un-minimizing,
#   3. its close button is still deleted afterwards, because a window the user can
#      close is a way to kill the agent by accident.
#
# Kills what it started, always. The real agent on 5003 is untouched.
$sig = @'
[DllImport("user32.dll")] public static extern bool EnumWindows(EnumProc cb, IntPtr p);
[DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr h);
[DllImport("user32.dll")] public static extern bool ShowWindow(IntPtr h, int n);
[DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int GetClassNameW(IntPtr h, System.Text.StringBuilder s, int n);
[DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int GetWindowTextW(IntPtr h, System.Text.StringBuilder s, int n);
[DllImport("user32.dll")] public static extern IntPtr GetSystemMenu(IntPtr h, bool revert);
[DllImport("user32.dll")] public static extern uint GetMenuState(IntPtr menu, uint id, uint flags);
public delegate bool EnumProc(IntPtr h, IntPtr p);
'@
$t = Add-Type -MemberDefinition $sig -Name RS -Namespace PR -PassThru | Where-Object { $_.Name -eq "RS" }

$script:hit = $null
$cb = [PR.RS+EnumProc]{
    param($h, $p)
    $sb = New-Object System.Text.StringBuilder 128
    $null = $t::GetClassNameW($h, $sb, 128)
    if ($sb.ToString() -eq "ConsoleWindowClass") {
        $tb = New-Object System.Text.StringBuilder 256
        $null = $t::GetWindowTextW($h, $tb, 256)
        if ($tb.ToString() -eq "llama.cpp agent") { $script:hit = $h }
    }
    return $true
}

& (Join-Path (Split-Path $PSScriptRoot -Parent) "run-agent.ps1") -Spawn --port $Port

try {
    $deadline = (Get-Date).AddSeconds(30)
    while ((Get-Date) -lt $deadline -and -not $script:hit) {
        $script:hit = $null; $null = $t::EnumWindows($cb, [IntPtr]::Zero); Start-Sleep -Milliseconds 100
    }
    if (-not $script:hit) { "agent window never appeared"; return }
    $h = $script:hit
    "found 0x{0:x}  visible_before={1}" -f [int64]$h, $t::IsWindowVisible($h)
    $null = $t::ShowWindow($h, 9)  # SW_RESTORE, what the tray's Show console does
    Start-Sleep -Milliseconds 400
    "after SW_RESTORE  visible={0}" -f $t::IsWindowVisible($h)
    # 0xF060 = SC_CLOSE; 4294967295 (-1) means the item is not in the menu at all.
    $state = $t::GetMenuState($t::GetSystemMenu($h, $false), 0xF060, 0x0)
    "SC_CLOSE state={0} (4294967295 = deleted)" -f $state
    $null = $t::ShowWindow($h, 0)  # SW_HIDE
    Start-Sleep -Milliseconds 200
    "after SW_HIDE     visible={0}" -f $t::IsWindowVisible($h)
}
finally {
    Get-CimInstance Win32_Process -Filter "Name='python.exe' OR Name='pwsh.exe' OR Name='conhost.exe'" |
        Where-Object { $_.CommandLine -like "*--port $Port*" } |
        ForEach-Object { "  killing pid $($_.ProcessId)"; Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
}
