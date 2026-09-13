param([string]$Out = "$env:TEMP\tray-menu.png", [int]$Width = 520, [int]$Height = 560)

# Pops the agent's tray menu without a human right-clicking, and screenshots it.
#
# This is the only way to see what the menu ACTUALLY offers. pystray's win32
# backend builds one HMENU and hands the same handle to every right-click, so
# iterating `icon.menu` from a test re-runs the `visible` callables and reports
# what they would say now, not what the live menu says. The two disagreed for a
# whole release.
#
# pystray registers WM_USER+11 as its icon callback and opens the menu at the
# CURRENT cursor position when the lparam is WM_RBUTTONUP. So park the cursor over
# the notification area, post the message, capture, dismiss with ESC, put the
# cursor back. `CopyFromScreen` rather than `PrintWindow`: a menu is its own
# top-level window and the point is what is on the glass.
Add-Type -AssemblyName System.Drawing
$src = @'
using System;
using System.Text;
using System.Runtime.InteropServices;
public class Tray {
  [StructLayout(LayoutKind.Sequential)] public struct RECT { public int Left, Top, Right, Bottom; }
  [StructLayout(LayoutKind.Sequential)] public struct POINT { public int X, Y; }
  public delegate bool EnumProc(IntPtr h, IntPtr l);
  [DllImport("user32.dll")] public static extern bool EnumWindows(EnumProc cb, IntPtr l);
  [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int GetClassNameW(IntPtr h, StringBuilder s, int n);
  [DllImport("user32.dll")] public static extern bool PostMessageW(IntPtr h, uint m, IntPtr w, IntPtr l);
  [DllImport("user32.dll")] public static extern bool SetCursorPos(int x, int y);
  [DllImport("user32.dll")] public static extern bool GetCursorPos(out POINT p);
  [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern IntPtr FindWindowW(string c, string n);
  [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern IntPtr FindWindowExW(IntPtr p, IntPtr c, string cls, string n);
  [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern bool SetProcessDpiAwarenessContext(IntPtr c);
  [DllImport("user32.dll")] public static extern void keybd_event(byte vk, byte scan, uint flags, IntPtr extra);
  // Found by window CLASS, not by process: pystray names its window
  // "<name><id(self)>SystemTrayIcon", and the agent runs in a CHILD python.exe of
  // the process whose command line mentions warden, so matching on the
  // command line finds the launcher and none of its windows.
  public static System.Collections.Generic.List<IntPtr> Find(string suffix) {
    var found = new System.Collections.Generic.List<IntPtr>();
    EnumWindows((h, l) => {
      var sb = new StringBuilder(256); GetClassNameW(h, sb, 256);
      if (sb.ToString().EndsWith(suffix)) found.Add(h);
      return true;
    }, IntPtr.Zero);
    return found;
  }
}
'@
Add-Type -TypeDefinition $src -Language CSharp
$null = [Tray]::SetProcessDpiAwarenessContext([IntPtr](-4))

$windows = [Tray]::Find("SystemTrayIcon")
if ($windows.Count -eq 0) { throw "no pystray window; is the agent running?" }
"tray windows: " + (($windows | ForEach-Object { $_.ToString() }) -join ', ')

$tray = [Tray]::FindWindowW("Shell_TrayWnd", $null)
$notify = [Tray]::FindWindowExW($tray, [IntPtr]::Zero, "TrayNotifyWnd", $null)
$tr = New-Object Tray+RECT; $null = [Tray]::GetWindowRect($notify, [ref]$tr)
$cx = [int](($tr.Left + $tr.Right) / 2); $cy = [int](($tr.Top + $tr.Bottom) / 2)

$was = New-Object Tray+POINT; $null = [Tray]::GetCursorPos([ref]$was)
$null = [Tray]::SetCursorPos($cx, $cy)
foreach ($h in $windows) { $null = [Tray]::PostMessageW($h, 0x040B, [IntPtr]0, [IntPtr]0x0205) }  # WM_USER+11, WM_RBUTTONUP
Start-Sleep -Milliseconds 900

# The menu opens up and to the left of the cursor, so the capture rect ends there.
$x0 = [Math]::Max(0, $cx - $Width); $y0 = [Math]::Max(0, $cy - $Height)
$bmp = New-Object System.Drawing.Bitmap($Width, $Height)
$g = [System.Drawing.Graphics]::FromImage($bmp)
$g.CopyFromScreen($x0, $y0, 0, 0, (New-Object System.Drawing.Size($Width, $Height)))
$bmp.Save($Out, [System.Drawing.Imaging.ImageFormat]::Png)
$g.Dispose(); $bmp.Dispose()

[Tray]::keybd_event(0x1B, 0, 0, [IntPtr]::Zero)      # ESC down
[Tray]::keybd_event(0x1B, 0, 2, [IntPtr]::Zero)      # ESC up
Start-Sleep -Milliseconds 200
$null = [Tray]::SetCursorPos($was.X, $was.Y)
"captured $x0,$y0 ${Width}x${Height} -> $Out"
