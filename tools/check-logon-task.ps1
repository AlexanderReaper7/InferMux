param([string]$TaskName = "EpistemeLlamaAgentFlashTest", [int]$Port = 5014, [int]$Seconds = 25)

# Registers a THROWAWAY scheduled task with the same action shape install-task.ps1
# writes, starts it the way logon would, and watches every ConsoleWindowClass window
# on the desktop for a flash.
#
# The thing being measured is a window that appears and is gone again in under a
# tenth of a second, which is exactly long enough to be infuriating at every logon
# and too short to catch by looking. `conhost --headless` is what stops it; this is
# what proves it stopped. Nothing here touches the real task or the real agent: its
# own task name, its own port, and a finally block that unregisters and kills.
$sig = @'
[DllImport("user32.dll")] public static extern bool EnumWindows(EnumProc cb, IntPtr p);
[DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr h);
[DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int GetClassNameW(IntPtr h, System.Text.StringBuilder s, int n);
[DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int GetWindowTextW(IntPtr h, System.Text.StringBuilder s, int n);
public delegate bool EnumProc(IntPtr h, IntPtr p);
'@
$t = Add-Type -MemberDefinition $sig -Name TT -Namespace PT -PassThru | Where-Object { $_.Name -eq "TT" }

$script:found = @{}
$script:titles = @{}
$cb = [PT.TT+EnumProc]{
    param($h, $p)
    $sb = New-Object System.Text.StringBuilder 128
    $null = $t::GetClassNameW($h, $sb, 128)
    if ($sb.ToString() -eq "ConsoleWindowClass") {
        $script:found[[int64]$h] = [bool]$t::IsWindowVisible($h)
        $tb = New-Object System.Text.StringBuilder 256
        $null = $t::GetWindowTextW($h, $tb, 256)
        $script:titles[[int64]$h] = $tb.ToString()
    }
    return $true
}
function Scan { $script:found = @{}; $script:titles = @{}; $null = $t::EnumWindows($cb, [IntPtr]::Zero) }

$runner = Join-Path (Split-Path $PSScriptRoot -Parent) "run-agent.ps1"
$action = New-ScheduledTaskAction -Execute "conhost.exe" `
    -Argument "--headless pwsh.exe -NoProfile -ExecutionPolicy Bypass -File `"$runner`" -Spawn --port $Port"
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero)
Register-ScheduledTask -TaskName $TaskName -Action $action -Settings $settings -Force | Out-Null

try {
    Scan
    $before = @($script:found.Keys)

    # 8 ms between scans: a flash of one frame at 120 Hz is 8 ms, and the whole
    # claim is that there is no frame at all.
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    Start-ScheduledTask -TaskName $TaskName
    $firstSeen = $null; $vis = @(); $last = @{}; $lastTitles = @{}
    while ($sw.ElapsedMilliseconds -lt ($Seconds * 1000)) {
        Scan
        foreach ($kv in $script:found.GetEnumerator()) {
            if ($before -contains $kv.Key) { continue }
            if ($null -eq $firstSeen) { $firstSeen = $sw.ElapsedMilliseconds }
            if ($kv.Value) { $vis += $sw.ElapsedMilliseconds }
            $last[$kv.Key] = $kv.Value
            $lastTitles[$kv.Key] = $script:titles[$kv.Key]
        }
        Start-Sleep -Milliseconds 8
    }

    "new_console_window_at=${firstSeen}ms"
    if ($vis.Count) { "FLASH: $($vis[0])ms -> $($vis[-1])ms = $($vis[-1] - $vis[0])ms over $($vis.Count) samples" }
    else { "NO FLASH: never observed visible" }
    foreach ($k in $last.Keys) { "  window 0x{0:x}  visible={1}  title='{2}'" -f $k, $last[$k], $lastTitles[$k] }

    # A window that never shows and an agent that never starts look identical from
    # the desktop, so the run is only good news if the port answers.
    try {
        $r = Invoke-RestMethod "http://127.0.0.1:$Port/status" -TimeoutSec 5
        "STATUS OK: $($r | ConvertTo-Json -Compress -Depth 4)"
    } catch { "STATUS FAILED: $($_.Exception.Message)" }
}
finally {
    Get-CimInstance Win32_Process -Filter "Name='python.exe' OR Name='pwsh.exe'" |
        Where-Object { $_.CommandLine -like "*llama_agent.py*--port $Port*" -or $_.CommandLine -like "*run-agent.ps1*--port $Port*" } |
        ForEach-Object { "  killing pid $($_.ProcessId)"; Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue
    "cleaned up"
}
