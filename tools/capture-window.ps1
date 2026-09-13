param(
    [string]$Title = "llama-warden",
    [string]$Class = "ConsoleWindowClass",
    [string]$Out = "$env:TEMP\agent-window.png"
)

# Screenshots one window by class and title, whether or not it is on top.
#
# DPI awareness FIRST, and that is the whole reason this file exists rather than a
# one-liner: without it the shell virtualises `GetWindowRect` for this process, so
# a 1815x921 window measures 877x493, the bitmap comes out that size, and
# `PrintWindow` renders only the top left corner into it. On screen that reads
# exactly like "the right hand side of the title bar is not being drawn", which is
# a bug in the app that does not exist.
#
# `PrintWindow` asks the window to paint itself, so it does not capture anything
# drawn on the SCREEN dc over the top. The minimize-to-tray wireframe
# (`DrawAnimatedRects`) is invisible here by construction, not by failure.
Add-Type -AssemblyName System.Drawing
$sig = @'
[DllImport("user32.dll")] public static extern bool PrintWindow(IntPtr h, IntPtr hdc, uint flags);
[DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
[DllImport("user32.dll", CharSet = CharSet.Unicode)] public static extern IntPtr FindWindowW(string c, string n);
[DllImport("user32.dll")] public static extern bool SetProcessDpiAwarenessContext(IntPtr ctx);
[StructLayout(LayoutKind.Sequential)] public struct RECT { public int Left, Top, Right, Bottom; }
'@
$t = Add-Type -MemberDefinition $sig -Name Cap -Namespace Win -PassThru | Where-Object { $_.Name -eq "Cap" }
$null = $t::SetProcessDpiAwarenessContext([IntPtr](-4))   # PER_MONITOR_AWARE_V2
$h = $t::FindWindowW($Class, $Title)
if ($h -eq [IntPtr]::Zero) { throw "no window '$Title' of class '$Class'" }
$r = New-Object Win.Cap+RECT
$null = $t::GetWindowRect($h, [ref]$r)
$width = $r.Right - $r.Left; $height = $r.Bottom - $r.Top
$bmp = New-Object System.Drawing.Bitmap($width, $height)
$g = [System.Drawing.Graphics]::FromImage($bmp)
$hdc = $g.GetHdc()
$ok = $t::PrintWindow($h, $hdc, 2)   # PW_RENDERFULLCONTENT
$g.ReleaseHdc($hdc)
$bmp.Save($Out, [System.Drawing.Imaging.ImageFormat]::Png)
$g.Dispose(); $bmp.Dispose()
"printwindow=$ok size=${width}x${height} -> $Out"
