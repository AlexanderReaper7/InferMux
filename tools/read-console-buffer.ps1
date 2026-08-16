param([int]$ProcessId, [string]$Out = "")

# Attaches to another process's console and reads its screen buffer as TEXT.
#
# This is what Textual actually painted, as opposed to what the window happens to
# show: conhost clips a final partial row when the window height is not a whole
# multiple of the cell height, so a widget can be drawn correctly and still look
# cut off. Text also diffs, which a screenshot does not.
#
# With no -ProcessId it finds the agent. Attaching means detaching from this
# script's own console first, which is why it prints through Write-Output at the
# end rather than as it goes.
$sig = @'
[DllImport("kernel32.dll", SetLastError=true)] public static extern bool FreeConsole();
[DllImport("kernel32.dll", SetLastError=true)] public static extern bool AttachConsole(uint pid);
[DllImport("kernel32.dll", SetLastError=true, CharSet=CharSet.Unicode)] public static extern IntPtr CreateFileW(string name, uint access, uint share, IntPtr sa, uint disp, uint flags, IntPtr tmpl);
[DllImport("kernel32.dll", SetLastError=true)] public static extern bool GetConsoleScreenBufferInfo(IntPtr h, out CSBI info);
[DllImport("kernel32.dll", SetLastError=true, CharSet=CharSet.Unicode)] public static extern bool ReadConsoleOutputCharacterW(IntPtr h, System.Text.StringBuilder buf, uint len, uint coord, out uint read);
[StructLayout(LayoutKind.Sequential)] public struct COORD { public short X, Y; }
[StructLayout(LayoutKind.Sequential)] public struct SMALL_RECT { public short Left, Top, Right, Bottom; }
[StructLayout(LayoutKind.Sequential)] public struct CSBI { public COORD Size; public COORD Cursor; public ushort Attr; public SMALL_RECT Window; public COORD Max; }
'@
$k = Add-Type -MemberDefinition $sig -Name Buf -Namespace Con -PassThru | Where-Object { $_.Name -eq "Buf" }

if (-not $ProcessId) {
    # The INNERMOST python.exe: uv re-executes itself, so there are two matching
    # processes and only the child ever paints. Picked as the one whose parent is
    # also a candidate rather than by pid order, which is not a tree.
    $candidates = @(Get-CimInstance Win32_Process |
        Where-Object { $_.Name -eq 'python.exe' -and $_.CommandLine -match 'llama_agent' })
    if (-not $candidates) { throw "agent not running and no -ProcessId given" }
    $parents = $candidates.ProcessId
    $leaf = $candidates | Where-Object { $parents -contains $_.ParentProcessId } | Select-Object -First 1
    $ProcessId = if ($leaf) { $leaf.ProcessId } else { $candidates[0].ProcessId }
}

$null = $k::FreeConsole()
if (-not $k::AttachConsole([uint32]$ProcessId)) {
    $code = [Runtime.InteropServices.Marshal]::GetLastWin32Error()
    throw "AttachConsole($ProcessId) failed: $([ComponentModel.Win32Exception]::new($code).Message)"
}

# GENERIC_READ|GENERIC_WRITE as an unsigned literal: PowerShell parses 0xC0000000
# as a negative Int32, and the marshaller then refuses to widen it to the uint the
# signature wants.
$h = $k::CreateFileW("CONOUT$", [uint32]3221225472, 3, [IntPtr]::Zero, 3, 0, [IntPtr]::Zero)
$info = New-Object Con.Buf+CSBI
$null = $k::GetConsoleScreenBufferInfo($h, [ref]$info)
$w = $info.Size.X
$lines = New-Object System.Collections.Generic.List[string]
$lines.Add("pid=$ProcessId buffer=$($info.Size.X)x$($info.Size.Y) window_rows=$($info.Window.Bottom - $info.Window.Top + 1)")
for ($y = $info.Window.Top; $y -le $info.Window.Bottom; $y++) {
    $sb = New-Object System.Text.StringBuilder $w
    $read = 0
    $null = $k::ReadConsoleOutputCharacterW($h, $sb, [uint32]$w, [uint32]($y * $w), [ref]$read)
    $lines.Add(("{0,3}| {1}" -f ($y - $info.Window.Top), $sb.ToString().TrimEnd()))
}
if ($Out) { $lines | Set-Content -Path $Out -Encoding utf8; "wrote $($lines.Count) lines -> $Out" }
else { $lines }
