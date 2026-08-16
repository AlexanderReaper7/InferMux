param([string]$Out = "$env:TEMP\taskbar.png", [int]$Width = 1400, [double]$Scale = 2)

# Screenshots the middle of the taskbar, which is where Windows 11 keeps the
# buttons. Written to check that the agent's button wears the agent's mark and not
# the icon of whatever shell opened its console (see `set_taskbar_identity`).
#
# The cursor is pushed into the bottom edge first because this taskbar auto-hides:
# `Shell_TrayWnd` then parks itself all but two pixels below the screen, so a
# capture of its own rect returns a sliver of wallpaper and looks like a blank
# taskbar. The rect is taken from `GetSystemMetrics`, i.e. the SCREEN, and only the
# bar's height is read from the window.
Add-Type -AssemblyName System.Drawing
$sig = @'
[DllImport("user32.dll", CharSet = CharSet.Unicode)] public static extern IntPtr FindWindowW(string c, string n);
[DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
[DllImport("user32.dll")] public static extern bool SetProcessDpiAwarenessContext(IntPtr ctx);
[DllImport("user32.dll")] public static extern bool SetCursorPos(int x, int y);
[DllImport("user32.dll")] public static extern bool GetCursorPos(out POINT p);
[DllImport("user32.dll")] public static extern int GetSystemMetrics(int i);
[StructLayout(LayoutKind.Sequential)] public struct RECT { public int Left, Top, Right, Bottom; }
[StructLayout(LayoutKind.Sequential)] public struct POINT { public int X, Y; }
'@
$t = Add-Type -MemberDefinition $sig -Name Bar -Namespace Cap -PassThru | Where-Object { $_.Name -eq "Bar" }
$null = $t::SetProcessDpiAwarenessContext([IntPtr](-4))
$sw = $t::GetSystemMetrics(0); $sh = $t::GetSystemMetrics(1)

# Parked and put back: the pointer belongs to whoever is at the keyboard.
$was = New-Object Cap.Bar+POINT; $null = $t::GetCursorPos([ref]$was)
$null = $t::SetCursorPos([int]($sw / 2), $sh - 1)
Start-Sleep -Milliseconds 900

$h = $t::FindWindowW("Shell_TrayWnd", $null)
$r = New-Object Cap.Bar+RECT
$null = $t::GetWindowRect($h, [ref]$r)
$bh = $r.Bottom - $r.Top
$y0 = $sh - $bh
$x0 = [int](($sw - $Width) / 2)
$bmp = New-Object System.Drawing.Bitmap($Width, $bh)
$g = [System.Drawing.Graphics]::FromImage($bmp)
$g.CopyFromScreen($x0, $y0, 0, 0, (New-Object System.Drawing.Size($Width, $bh)))
$ow = [int]($Width * $Scale); $oh = [int]($bh * $Scale)
$dst = New-Object System.Drawing.Bitmap($ow, $oh)
$g2 = [System.Drawing.Graphics]::FromImage($dst)
$g2.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::NearestNeighbor
$g2.DrawImage($bmp, 0, 0, $ow, $oh)
$dst.Save($Out, [System.Drawing.Imaging.ImageFormat]::Png)
$g.Dispose(); $g2.Dispose(); $bmp.Dispose(); $dst.Dispose()
$null = $t::SetCursorPos($was.X, $was.Y)
"screen ${sw}x${sh}  taskbar height $bh  captured $x0,$y0 ${Width}x${bh} x$Scale -> $Out"
