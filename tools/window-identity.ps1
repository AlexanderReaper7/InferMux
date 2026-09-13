param(
    [string]$Title = "llama-warden",
    [string]$Class = "ConsoleWindowClass",
    [string]$SetId = "",
    [string]$SetIcon = "",
    [int]$Clear = 0,
    [switch]$Recreate
)

# Reads and writes the AppUserModel properties that decide what a window's TASKBAR
# BUTTON looks like. Written to diagnose, and then to prove, the split that
# `set_taskbar_identity` fixes: `WM_SETICON` reaches the title bar, the window menu,
# Alt-Tab and the hover thumbnail, and reaches the taskbar button not at all. The
# button is drawn from the identity the shell resolves for the window, and a console
# window resolves to the process that opened it, i.e. pwsh.exe.
#
# -Recreate hides and re-shows the window. That is not cosmetic: the identity is
# read WHEN THE BUTTON IS CREATED and never again, so setting it on a window that
# already has one changes nothing until the button is destroyed.
#
# Measured with this script on 2026-08-16: an AppUserModelID alone is enough. With
# `RelaunchIconResource` cleared and an ID never paired with one, the button still
# drew the window icon. Hence the agent sets one property, not four.
$src = @'
using System;
using System.Runtime.InteropServices;

[StructLayout(LayoutKind.Sequential, Pack = 4)]
public struct PROPERTYKEY { public Guid fmtid; public uint pid; }

[StructLayout(LayoutKind.Explicit, Size = 24)]
public struct PROPVARIANT {
  [FieldOffset(0)] public ushort vt;
  [FieldOffset(8)] public IntPtr p;
}

[ComImport, Guid("886d8eeb-8cf2-4446-8d02-cdba1dbdcf99"),
 InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
public interface IPropertyStore {
  void GetCount(out uint c);
  void GetAt(uint i, out PROPERTYKEY key);
  void GetValue(ref PROPERTYKEY key, out PROPVARIANT v);
  void SetValue(ref PROPERTYKEY key, ref PROPVARIANT v);
  void Commit();
}

public class WinProps {
  [DllImport("shell32.dll")] public static extern int SHGetPropertyStoreForWindow(
      IntPtr hwnd, ref Guid iid, [MarshalAs(UnmanagedType.Interface)] out IPropertyStore ps);
  [DllImport("user32.dll", CharSet = CharSet.Unicode)] public static extern IntPtr FindWindowW(string c, string n);
  [DllImport("user32.dll")] public static extern bool ShowWindow(IntPtr h, int cmd);
  [DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr h);

  static Guid APPMODEL = new Guid("9F4C2855-9F79-4B39-A8D0-E1D42DE1D5F3");
  public static PROPERTYKEY Key(uint pid) { var k = new PROPERTYKEY(); k.fmtid = APPMODEL; k.pid = pid; return k; }

  public static IPropertyStore Store(IntPtr hwnd) {
    var iid = new Guid("886d8eeb-8cf2-4446-8d02-cdba1dbdcf99");
    IPropertyStore ps; int hr = SHGetPropertyStoreForWindow(hwnd, ref iid, out ps);
    if (hr != 0) throw new Exception("SHGetPropertyStoreForWindow hr=0x" + hr.ToString("X8"));
    return ps;
  }

  public static string Read(IntPtr hwnd, uint pid) {
    var ps = Store(hwnd); var k = Key(pid); PROPVARIANT v;
    ps.GetValue(ref k, out v);
    if (v.vt == 31 || v.vt == 30) return Marshal.PtrToStringUni(v.p);
    return v.vt == 0 ? "(unset)" : "(vt=" + v.vt + ")";
  }

  public static void Erase(IntPtr hwnd, uint pid) {
    var ps = Store(hwnd); var k = Key(pid);
    var v = new PROPVARIANT();   // VT_EMPTY removes the property
    ps.SetValue(ref k, ref v);
    ps.Commit();
  }

  public static void Write(IntPtr hwnd, uint pid, string value) {
    var ps = Store(hwnd); var k = Key(pid);
    var v = new PROPVARIANT();
    v.vt = 31;                   // VT_LPWSTR
    v.p = Marshal.StringToCoTaskMemUni(value);
    ps.SetValue(ref k, ref v);   // SetValue copies, so the string is ours to leak or not
    ps.Commit();
  }
}
'@
Add-Type -TypeDefinition $src -Language CSharp

$hwnd = [WinProps]::FindWindowW($Class, $Title)
if ($hwnd -eq [IntPtr]::Zero) { throw "no window '$Title' of class '$Class'" }
"hwnd=$hwnd visible=$([WinProps]::IsWindowVisible($hwnd))"

if ($Clear) { [WinProps]::Erase($hwnd, [uint32]$Clear); "cleared pid $Clear" }
if ($SetId) { [WinProps]::Write($hwnd, 5, $SetId); "set AppUserModel.ID = $SetId" }
if ($SetIcon) { [WinProps]::Write($hwnd, 8, $SetIcon); "set RelaunchIconResource = $SetIcon" }

foreach ($pair in @(@(5, 'ID'), @(8, 'RelaunchIconResource'),
                    @(2, 'RelaunchCommand'), @(4, 'RelaunchDisplayNameResource'),
                    @(9, 'PreventPinning'))) {
    try { "{0,-28} = {1}" -f $pair[1], [WinProps]::Read($hwnd, [uint32]$pair[0]) }
    catch { "{0,-28} ! {1}" -f $pair[1], $_.Exception.Message }
}

if ($Recreate) {
    $null = [WinProps]::ShowWindow($hwnd, 0)   # SW_HIDE destroys the taskbar button
    Start-Sleep -Milliseconds 400
    $null = [WinProps]::ShowWindow($hwnd, 5)   # SW_SHOW makes a new one, reading the identity
    "recreated taskbar button"
}
