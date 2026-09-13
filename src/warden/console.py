"""Tray icon, console-window control and a text UI for the warden.

The warden has always been correct and invisible. What it was not is *present*:
llama-server ran in whichever terminal the launcher was typed into, the embed
server owned a minimized taskbar button of its own, and the service had no face
at all. Three desktop artefacts for one logical thing, and the only way to read a
log was to have Docker up and a consumer's admin page open, which is precisely
not the case when something is wrong.

So both servers go headless (they already write everything to `logs/*.log`) and
this module becomes the one window: a tray icon that is also a status light, and
a console you show and hide from it.

**It is injected, never imported into.** `AgentAPI` below is the entire surface,
and `__main__.main` fills it in. That keeps the UI drivable by a test against
seven lambdas, with no GPU, no llama-server and no policy loop in the process.

Windows notes, both learned the hard way and both load-bearing:

* `GetConsoleWindow()` returns whatever console this process is ATTACHED to, not
  one it owns. Run by hand from a terminal, that is the user's terminal, so
  hiding it or deleting its close button would vandalize a window we were merely
  borrowing. Hence `owns_console`: the destructive calls happen only when the
  caller passed `--hide`, which only the scheduled task does.
* Under Windows 11 the default terminal is Windows Terminal, where the console
  window is a pseudo-console the real UI does not live in and `ShowWindow` moves
  nothing the eye can see. `install-task.ps1` therefore launches through
  `conhost.exe` explicitly, which is the classic host these calls were designed
  against.
"""

from __future__ import annotations

import ctypes
import logging
import os
import tempfile
import threading
from collections import deque
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from typing import ClassVar

import pystray
from PIL import Image, ImageDraw
from rich.segment import Segments
from textual import events, work
from textual.app import App, ComposeResult
from textual.binding import Binding
from textual.containers import Horizontal
from textual.scrollbar import ScrollBar, ScrollBarRender
from textual.theme import Theme
from textual.widgets import RichLog, Static, TabbedContent, TabPane

log = logging.getLogger("warden.console")

# The palette, inherited token for token from Episteme's web/static/style.css so this
# window and the admin page are recognisably the same program - and so the tray
# light and a status chip agree about what green means. Black is #000000 on
# purpose, not a near-black: the project is OLED-first everywhere.
BG = "#000000"  # --bg
SURFACE = "#101014"  # --card-bg
BORDER = "#1e1f26"  # --border
TEXT = "#e8e6e1"  # --text
MUTED = "#8f95a3"  # --muted
ACCENT = "#7aa2f7"  # --accent
OK_HEX = "#17d98e"  # --ok
WARN_HEX = "#e0af68"  # --warn
BAD_HEX = "#c7082f"  # --bad


def _rgb(value: str) -> tuple[int, int, int]:
    return (int(value[1:3], 16), int(value[3:5], 16), int(value[5:7], 16))


# The icon is drawn with Pillow, which wants tuples, so these are the same tokens
# in the other notation rather than a second set of colours. `down` is MUTED grey
# and not crimson deliberately: a stopped backend is the normal overnight state,
# not a fault, and an icon that cries wolf stops being read by the second week.
OK = _rgb(OK_HEX)
DOWN = _rgb(MUTED)
BAD = _rgb(BAD_HEX)

# Display names. The key stays the agent's own (`SERVERS`, the log file stem, the
# `#log-*` id); only what a person reads changes. "router" alone names the
# plumbing and not the job: :5001 is where `main` and `fast` live, swapped in and
# out of VRAM on demand, which is the thing you are watching when you open the tab.
LABELS = {"router": "router/main"}


def label(name: str) -> str:
    return LABELS.get(name, name)


def percent(value: object) -> str:
    """A utilization figure, or "-" when there is not one.

    `dict.get(key, "-")` is not enough and that is the whole reason this exists:
    `/resources` always emits `gpu_percent`, and its value is `null` whenever
    nvidia-smi reported no utilization line. The default covers a missing key,
    the key is never missing, and the status bar read `None%`. A reader cannot
    tell that from a real measurement of zero."""
    return "-" if value is None else f"{value}%"


# --- the seam ----------------------------------------------------------------------


@dataclass
class AgentAPI:
    """Everything the UI is allowed to do to the agent.

    Deliberately callables rather than the module, so this file states its
    dependencies instead of reaching for whatever happens to be in scope, and so
    a test can drive the whole UI against seven lambdas.

    `verdict` returns the watcher's `as_dict()`: the last measurement AND what
    was decided from it. The UI does not measure. Calling `agent.resources()`
    from here would run a second ~3.5 s counter sweep beside the watch thread's
    own, on a box where the entire purpose of that sweep is to notice that the
    GPU is busy.
    """

    server_status: Callable[[], dict[str, dict]]
    verdict: Callable[[], dict]
    read_log: Callable[..., dict]
    start: Callable[[], dict]
    stop: Callable[[], dict]
    restart: Callable[[], dict]
    servers: dict[str, int]
    log_dir: Path
    records: deque[str]


# --- win32 console control ---------------------------------------------------------

SW_HIDE, SW_SHOW, SW_RESTORE = 0, 5, 9
MF_BYCOMMAND = 0x0000
SC_CLOSE = 0xF060

GWL_STYLE = -16
WS_CAPTION = 0x00C00000  # WS_BORDER | WS_DLGFRAME: the title bar and its border
WS_VSCROLL = 0x00200000
WS_HSCROLL = 0x00100000
SWP_NOSIZE, SWP_NOMOVE, SWP_NOZORDER, SWP_FRAMECHANGED = 0x0001, 0x0002, 0x0004, 0x0020
WM_SETICON = 0x0080
ICON_SMALL, ICON_BIG = 0, 1
IMAGE_ICON = 1
LR_LOADFROMFILE = 0x0010
VT_LPWSTR = 31
# `PKEY_AppUserModel_ID`, pid 5 of the AppUserModel property set. The string is
# arbitrary and never registered anywhere: its only job is to be different from
# the one the shell would infer.
APPMODEL_FMTID = (0x9F4C2855, 0x9F79, 0x4B39, (0xA8, 0xD0, 0xE1, 0xD4, 0x2D, 0xE1, 0xD5, 0xF3))
PKEY_APPUSERMODEL_ID = 5
TASKBAR_APP_ID = "LlamaWarden"
STD_OUTPUT_HANDLE = -11
IDANI_CAPTION = 3
SPI_GETANIMATION = 0x0048
FORCE_DARK = 2  # PreferredAppMode: Default 0, AllowDark 1, ForceDark 2, ForceLight 3

_kernel32 = ctypes.windll.kernel32 if os.name == "nt" else None
_user32 = ctypes.windll.user32 if os.name == "nt" else None
_shell32 = ctypes.windll.shell32 if os.name == "nt" else None

if _user32 is not None:
    # Declared, not left to ctypes' defaults, for the calls that carry a HANDLE or
    # an LPARAM. The default restype is `c_int`: a 64-bit HICON comes back with its
    # top half cut off, and the truncated value is a plausible-looking integer that
    # simply names no icon. Nothing raises; the window just keeps the old picture.
    _user32.LoadImageW.restype = ctypes.c_void_p
    _user32.LoadImageW.argtypes = [
        ctypes.c_void_p, ctypes.c_wchar_p, ctypes.c_uint,
        ctypes.c_int, ctypes.c_int, ctypes.c_uint,
    ]
    _user32.SendMessageW.restype = ctypes.c_void_p
    _user32.SendMessageW.argtypes = [
        ctypes.c_void_p, ctypes.c_uint, ctypes.c_void_p, ctypes.c_void_p
    ]
    _user32.GetSystemMenu.restype = ctypes.c_void_p
    _user32.GetSystemMenu.argtypes = [ctypes.c_void_p, ctypes.c_bool]
    _user32.DeleteMenu.argtypes = [ctypes.c_void_p, ctypes.c_uint, ctypes.c_uint]


class _Rect(ctypes.Structure):
    _fields_ = [("left", ctypes.c_long), ("top", ctypes.c_long),
                ("right", ctypes.c_long), ("bottom", ctypes.c_long)]


class _Point(ctypes.Structure):
    _fields_ = [("x", ctypes.c_long), ("y", ctypes.c_long)]


class _Coord(ctypes.Structure):
    _fields_ = [("x", ctypes.c_short), ("y", ctypes.c_short)]


class _SmallRect(ctypes.Structure):
    _fields_ = [("left", ctypes.c_short), ("top", ctypes.c_short),
                ("right", ctypes.c_short), ("bottom", ctypes.c_short)]


class _BufferInfo(ctypes.Structure):
    _fields_ = [("size", _Coord), ("cursor", _Coord), ("attributes", ctypes.c_ushort),
                ("window", _SmallRect), ("max_window", _Coord)]


class _AnimationInfo(ctypes.Structure):
    _fields_ = [("cb_size", ctypes.c_uint), ("min_animate", ctypes.c_int)]


class _Guid(ctypes.Structure):
    _fields_ = [("data1", ctypes.c_uint32), ("data2", ctypes.c_uint16),
                ("data3", ctypes.c_uint16), ("data4", ctypes.c_ubyte * 8)]


class _PropertyKey(ctypes.Structure):
    _fields_ = [("fmtid", _Guid), ("pid", ctypes.c_ulong)]


class _PropVariant(ctypes.Structure):
    """The 24 bytes of a PROPVARIANT, with only the two fields this file writes.

    The union is the last 16, and `VT_LPWSTR` uses the first 8 of them for a
    pointer. Declaring the tail anyway matters: `SetValue` reads the whole
    structure, so a short one hands it whatever is next on the stack.
    """

    _fields_ = [("vt", ctypes.c_ushort), ("reserved1", ctypes.c_ushort),
                ("reserved2", ctypes.c_ushort), ("reserved3", ctypes.c_ushort),
                ("value", ctypes.c_void_p), ("tail", ctypes.c_void_p)]


if _user32 is not None:
    # Second block only because these take the structures above. Same reason as
    # the first: a truncated HWND names no window, and silently.
    _user32.FindWindowW.restype = ctypes.c_void_p
    _user32.FindWindowW.argtypes = [ctypes.c_wchar_p, ctypes.c_wchar_p]
    _user32.FindWindowExW.restype = ctypes.c_void_p
    _user32.FindWindowExW.argtypes = [
        ctypes.c_void_p, ctypes.c_void_p, ctypes.c_wchar_p, ctypes.c_wchar_p
    ]
    _user32.GetWindowRect.argtypes = [ctypes.c_void_p, ctypes.POINTER(_Rect)]
    _user32.DrawAnimatedRects.argtypes = [
        ctypes.c_void_p, ctypes.c_int, ctypes.POINTER(_Rect), ctypes.POINTER(_Rect)
    ]


def console_hwnd() -> int:
    """HWND of the attached console, or 0 when there is none (redirected output,
    a service with no console, anything not Windows)."""
    return int(_kernel32.GetConsoleWindow()) if _kernel32 else 0


def minimize_animation_enabled() -> bool:
    """Windows' own "animate windows when minimizing and maximizing" switch.

    Read rather than assumed, because a person who has turned window animation
    off has said what they want and an app that animates anyway is ignoring the
    setting it is imitating.
    """
    if not _user32:
        return False
    info = _AnimationInfo(ctypes.sizeof(_AnimationInfo), 0)
    if not _user32.SystemParametersInfoW(
        SPI_GETANIMATION, ctypes.sizeof(info), ctypes.byref(info), 0
    ):
        return False
    return bool(info.min_animate)


def tray_rect() -> _Rect | None:
    """The notification area, as somewhere to fly into.

    The area and not this icon's own cell within it: the precise rect needs
    `Shell_NotifyIconGetRect` with the icon's `hWnd` and `uID`, which are
    pystray's private business, and the animation is a gesture rather than a
    measurement - Windows aims its own at the taskbar button, not at a pixel.
    """
    if not _user32:
        return None
    tray = _user32.FindWindowW("Shell_TrayWnd", None)
    notify = _user32.FindWindowExW(tray, None, "TrayNotifyWnd", None) if tray else None
    rect = _Rect()
    if notify and _user32.GetWindowRect(notify, ctypes.byref(rect)):
        return rect
    return None


def animate_to_tray(hwnd: int, reverse: bool = False) -> bool:
    """The wireframe zoom Windows plays on minimize, aimed at the tray.

    `DrawAnimatedRects` is that animation, exposed as an API. It draws on the
    screen DC and never touches the window, so the worst failure available here
    is no animation.

    An iconic window is skipped, which is what makes one rule cover both ways in.
    Hidden from the title bar, the `h` key or the tray menu, the window is on
    screen and its rect is the shape to shrink; hidden because the person
    minimized it, Windows has ALREADY animated it into the taskbar and the rect
    is off at -32000, so a second animation would fly from nowhere. Restoring
    reads the same rect for the same reason, and runs before the window is shown.
    """
    if not (hwnd and _user32) or _user32.IsIconic(hwnd):
        return False
    target = tray_rect()
    if target is None or not minimize_animation_enabled():
        return False
    window = _Rect()
    if not _user32.GetWindowRect(hwnd, ctypes.byref(window)):
        return False
    start, end = (target, window) if reverse else (window, target)
    return bool(
        _user32.DrawAnimatedRects(
            hwnd, IDANI_CAPTION, ctypes.byref(start), ctypes.byref(end)
        )
    )


def show_console() -> None:
    hwnd = console_hwnd()
    if hwnd and _user32:
        animate_to_tray(hwnd, reverse=True)
        _user32.ShowWindow(hwnd, SW_RESTORE)
        _user32.SetForegroundWindow(hwnd)


def hide_console() -> None:
    hwnd = console_hwnd()
    if hwnd and _user32:
        animate_to_tray(hwnd)
        _user32.ShowWindow(hwnd, SW_HIDE)


def console_is_visible() -> bool:
    """Whether the window is on screen, which for this app means "not in the
    tray": hiding to the tray IS `SW_HIDE`, so `IsWindowVisible` is the question
    the tray menu is asking when it decides between Show and Hide."""
    hwnd = console_hwnd()
    return bool(_user32.IsWindowVisible(hwnd)) if (hwnd and _user32) else False


def enable_dark_menus() -> bool:
    """Make Win32 popup menus - the tray's right-click menu - dark.

    A `TrackPopupMenu` menu is drawn by the system, not by the app, and it stays
    light until the process opts in through two uxtheme entry points that Windows
    exports **by ordinal only**: 135 `SetPreferredAppMode` and 136
    `FlushMenuThemes`. `ForceDark` rather than `AllowDark` because this program is
    dark whatever the system is set to, which is the same choice the web UI makes.

    Undocumented and unnamed, so a future Windows may renumber them; the whole
    call is therefore best-effort and its failure is a light menu, nothing more.
    """
    if os.name != "nt":
        return False
    try:
        uxtheme = ctypes.windll.uxtheme
        set_preferred_app_mode = uxtheme[135]
        set_preferred_app_mode.argtypes = [ctypes.c_int]
        set_preferred_app_mode.restype = ctypes.c_int
        set_preferred_app_mode(FORCE_DARK)
        uxtheme[136]()  # FlushMenuThemes, or the change lands only on new menus
    except Exception as exc:  # noqa: BLE001 - a light menu is not worth an exit
        log.debug("dark menus unavailable: %s", exc)
        return False
    return True


def disable_console_close() -> None:
    """Grey out the console's X.

    Not cosmetic. A console close sends `CTRL_CLOSE_EVENT`, which a handler may
    observe but **may not cancel** - Windows gives roughly five seconds and then
    kills the process regardless. So the only way for "hide to tray" to be the
    real behaviour of the close button is for there to be no close button. Quit
    stays available from the tray menu and from `q`.
    """
    hwnd = console_hwnd()
    if hwnd and _user32:
        menu = _user32.GetSystemMenu(hwnd, False)
        if menu:
            _user32.DeleteMenu(menu, SC_CLOSE, MF_BYCOMMAND)


def set_console_title(title: str) -> None:
    """Still set even though the caption that displayed it is gone: the title is
    how `open-agent.ps1` finds this window, matched with the class through
    `FindWindowW`. It is an identifier now rather than a label."""
    if _kernel32:
        _kernel32.SetConsoleTitleW(title)


def frameless_style(style: int) -> int:
    """The window style with the host's own chrome taken out.

    Pure, so what is stripped and what is kept is a fact a test can hold. Kept on
    purpose: `WS_THICKFRAME`, which is the sizing border. A borderless window that
    cannot be resized would be a log viewer you cannot make bigger, and Windows
    draws that border as a single pixel with no caption attached to it.

    Stripped: the caption, because the app draws its own title row and two of them
    is one too many; and both scrollbars, because the log has a scrollbar of its
    own and the console's sits directly beside it scrolling something else.
    """
    return style & ~(WS_CAPTION | WS_VSCROLL | WS_HSCROLL)


def strip_console_frame() -> None:
    hwnd = console_hwnd()
    if not (hwnd and _user32):
        return
    style = _user32.GetWindowLongW(hwnd, GWL_STYLE)
    _user32.SetWindowLongW(hwnd, GWL_STYLE, frameless_style(style))
    # The frame is cached until something says otherwise, and SWP_FRAMECHANGED is
    # that something. Without it the caption stays painted until the next resize.
    _user32.SetWindowPos(
        hwnd, 0, 0, 0, 0, 0, SWP_NOMOVE | SWP_NOSIZE | SWP_NOZORDER | SWP_FRAMECHANGED
    )


def match_console_buffer() -> bool:
    """Shrink the screen buffer to the visible window. True if it moved.

    This, not the style bit above, is what actually removes conhost's scrollbar:
    the bar exists because the buffer is 9001 rows and the window shows 30 of
    them. Clearing `WS_VSCROLL` alone hides a control that the host will put back
    the next time it recalculates its frame. No buffer to scroll, no scrollbar,
    and no console scrollback either - which is correct here, because the thing
    worth scrolling back through is the log pane, and it keeps its own 5000 lines.
    """
    if not _kernel32:
        return False
    handle = _kernel32.GetStdHandle(STD_OUTPUT_HANDLE)
    info = _BufferInfo()
    if not _kernel32.GetConsoleScreenBufferInfo(handle, ctypes.byref(info)):
        return False
    width = info.window.right - info.window.left + 1
    height = info.window.bottom - info.window.top + 1
    if (info.size.x, info.size.y) == (width, height):
        return False
    return bool(_kernel32.SetConsoleScreenBufferSize(handle, _Coord(width, height)))


def console_is_minimized() -> bool:
    hwnd = console_hwnd()
    return bool(_user32.IsIconic(hwnd)) if (hwnd and _user32) else False


def console_window_origin() -> tuple[int, int]:
    hwnd = console_hwnd()
    rect = _Rect()
    if hwnd and _user32 and _user32.GetWindowRect(hwnd, ctypes.byref(rect)):
        return (rect.left, rect.top)
    return (0, 0)


def move_console_window(x: int, y: int) -> None:
    hwnd = console_hwnd()
    if hwnd and _user32:
        _user32.SetWindowPos(hwnd, 0, int(x), int(y), 0, 0, SWP_NOSIZE | SWP_NOZORDER)


def cursor_position() -> tuple[int, int]:
    """Screen pixels. The drag needs a resolution the terminal does not have: a
    mouse event arrives in character cells, so a window dragged by cell would jump
    in ~8 px steps and never come to rest where the pointer is."""
    point = _Point()
    if _user32 and _user32.GetCursorPos(ctypes.byref(point)):
        return (point.x, point.y)
    return (0, 0)


# Kept alive for the life of the process. `WM_SETICON` does not copy the icon, it
# stores the handle, so a garbage-collected HICON leaves the window pointing at
# freed memory - which Windows renders as the default icon, i.e. exactly the bug
# this is here to fix, arriving several seconds later.
_window_icons: list[int] = []


def set_console_icon(path: Path) -> bool:
    """Give the console window our mark, small and large.

    Without this the taskbar button and Alt-Tab show pwsh.exe's icon, because a
    console window has no icon of its own and the shell falls back to the image of
    whatever process is attached - which is the launcher, not the agent.

    Two sizes because Windows asks for two: `ICON_SMALL` is the 16 px one used for
    Alt-Tab's small mode and the window menu, `ICON_BIG` the 32 px one the taskbar
    scales. Loading each at its own size lets the .ico's per-size renders be used
    rather than one bitmap resampled twice.
    """
    hwnd = console_hwnd()
    if not (hwnd and _user32):
        return False
    loaded = False
    for which, size in ((ICON_SMALL, 16), (ICON_BIG, 32)):
        handle = _user32.LoadImageW(None, str(path), IMAGE_ICON, size, size, LR_LOADFROMFILE)
        if handle:
            _user32.SendMessageW(hwnd, WM_SETICON, ctypes.c_void_p(which), handle)
            _window_icons.append(handle)
            loaded = True
    return loaded


def set_taskbar_identity(app_id: str = TASKBAR_APP_ID) -> bool:
    """Give the console window an AppUserModelID of its own.

    `set_console_icon` is not enough on its own, and the split is visible: with
    only the icon set, the title bar, the window menu and the hover thumbnail
    show our mark while the TASKBAR BUTTON keeps showing pwsh's. The button is
    not drawn from the window icon at all. It is drawn from the identity the
    shell resolves for the window, and a console window resolves to the process
    that opened it, which is the launcher.

    Setting the property makes the window its own app rather than another pwsh
    window, and the button then falls back to the window icon, which by then is
    ours. Nothing else is needed - measured 2026-08-16 by clearing
    `RelaunchIconResource` and using an AppUserModelID never paired with one: the
    button still drew the mark. So the picture stays in exactly one place.

    **The window must not have a taskbar button when this runs.** The identity is
    read when the button is created, and never again for the life of it. In
    `run` that is free, because `adopt_console_window` is followed immediately by
    `hide_console`, so the first button any of this produces is the one made by
    the first Show.
    """
    hwnd = console_hwnd()
    if not (hwnd and _shell32):
        return False
    iid = _Guid(0x886D8EEB, 0x8CF2, 0x4446,
                (0x8D, 0x02, 0xCD, 0xBA, 0x1D, 0xBD, 0xCF, 0x99))  # IPropertyStore
    store = ctypes.c_void_p()
    if _shell32.SHGetPropertyStoreForWindow(
        ctypes.c_void_p(hwnd), ctypes.byref(iid), ctypes.byref(store)
    ) or not store:
        return False
    # No comtypes on this program's dependency list for one interface with two
    # methods used: the vtable is walked by hand. Slots are IUnknown's three then
    # IPropertyStore's own order, so Release is 2, SetValue 6 and Commit 7.
    vtable = ctypes.cast(store, ctypes.POINTER(ctypes.POINTER(ctypes.c_void_p))).contents
    release = ctypes.WINFUNCTYPE(ctypes.c_ulong, ctypes.c_void_p)(vtable[2])
    try:
        set_value = ctypes.WINFUNCTYPE(
            ctypes.c_long, ctypes.c_void_p,
            ctypes.POINTER(_PropertyKey), ctypes.POINTER(_PropVariant),
        )(vtable[6])
        commit = ctypes.WINFUNCTYPE(ctypes.c_long, ctypes.c_void_p)(vtable[7])
        key = _PropertyKey(_Guid(*APPMODEL_FMTID), PKEY_APPUSERMODEL_ID)
        text = ctypes.create_unicode_buffer(app_id)
        # `SetValue` copies the variant, so the buffer only has to outlive the
        # call, and there is no PropVariantClear to answer for.
        value = _PropVariant(vt=VT_LPWSTR, value=ctypes.cast(text, ctypes.c_void_p))
        if set_value(store, ctypes.byref(key), ctypes.byref(value)):
            return False
        return not commit(store)
    finally:
        release(store)


# --- the tray icon is the mark, and the mark is a status light -----------------------

# The warden's mark, from `graphics/warden-icon.svg`: an obelisk standing in a
# neural network that sags toward its lower tip, because the structure bends what
# it stands in. Inherited from Episteme, whose mark is the same solid in a field
# of plain lines, and free to diverge from it since the split.
#
# The facet greys are the generator's output, read off the four `<path>` fills it
# emits, so the taskbar and the design tree cannot drift into two different
# obelisks. They are NOT imported: `graphics/build_svg.py` is a uv script
# with a header of its own and no relationship to the agent's dependencies, and
# an agent that could not draw its own icon without the design folder present
# would be a worse agent.
MARK_LIT_UPPER = (162, 170, 180)
MARK_LIT_LOWER = (123, 130, 138)
MARK_DARK_UPPER = (46, 48, 51)
MARK_DARK_LOWER = (41, 43, 46)

# Layout in a unit box, read off the same four `<path>`s as the greys and divided
# by the 256 viewBox, so the tray obelisk stands exactly where the generator puts
# it. The camera is the generator's, copied with it at the split. The lower
# half lands INSIDE the network and hides part of it: standing clear of the net
# would say the two are adjacent, and the occlusion is the "embedded in the field"
# claim made by overlap.
MARK_APEX_Y = 14.69 / 256
MARK_EQUATOR_Y = 119.76 / 256
MARK_NEAR_Y = 128.77 / 256
MARK_BASE_Y = 216.67 / 256
MARK_HALF_W = (170.81 - 128.0) / 256

# The network. `NET_BAND` is where it may be drawn, as a fraction of the box, and
# every node is placed against those bounds with its own radius already subtracted
# - so no arrangement can be clipped by the frame, whatever the numbers below say.
# The band is over half the tile because that is where the room is. The solid
# cannot grow: its lower tip has to land in the sag, that contact IS the claim,
# and the sag is already near the bottom of the frame - so zooming the camera
# drives the tip through the field and out the floor (measured: fov 26 puts it at
# 95% of the height, past the lower rank). Widening the FIELD instead fills the
# two flanks either side of the obelisk's upper half, which were the empty part
# of the tile, and it says the same thing louder: a wide field bending into a well.
NET_BAND = (0.46, 0.98)
NET_RADIUS = 0.045
NET_EDGE_W = 0.018
NET_UPPER = 5  # the rank the obelisk deforms
NET_LOWER = 3
NET_REACH = 0.34  # an edge is drawn between nodes closer than this in x
NET_DIP = 0.72  # how far the upper rank sags toward the lower, 0..1 of the gap
NET_CLEARANCE = 2.4  # node radii the dip must leave between the two ranks

SUPERSAMPLE = 4  # drawn large and downsampled: Pillow has no antialiased polygon


def icon_colors(status: dict[str, dict], names: list[str]) -> list[tuple[int, int, int]]:
    """One colour per server, in `names` order. Pure, so the thing the eye
    actually reads off the taskbar is a unit test rather than a screenshot."""
    return [OK if (status.get(name) or {}).get("listening") else DOWN for name in names]


def _legible(color: tuple[int, int, int], floor: int = 92) -> tuple[int, int, int]:
    """Lift a facet until its brightest channel clears `floor`, keeping the ratios.

    The mark's unlit facets are near-black because it is drawn for a page with a
    silhouette to read against. A taskbar is black, so unlifted they vanish and
    the obelisk arrives as half an obelisk - lit side only, which reads as a
    triangle. Scaling rather than clamping keeps the shading a shading.
    """
    peak = max(color) or 1
    if peak >= floor:
        return color
    scale = floor / peak
    r, g, b = (min(255, round(channel * scale)) for channel in color)
    return (r, g, b)


Point = tuple[float, float]


def net_layout(big: float) -> tuple[list[Point], list[Point], float]:
    """Where the network's nodes go in a `big`-square box, and their radius.

    Pure, and separate from the drawing, because both things it has to guarantee
    are arithmetic and can therefore be tested instead of squinted at:

    * **nothing leaves the frame.** Every node is placed against bounds that
      already have its own radius subtracted, so the constants above cannot
      produce a clipped icon however they are set.
    * **the ranks do not collide.** How far the upper rank sags is CLAMPED
      against `NET_CLEARANCE`, not chosen. At the depth that reads best the
      centre node overlaps the node beneath it, and a depth tuned until the two
      happen to clear is a number that stops clearing the moment the band moves.
    """
    radius = NET_RADIUS * big
    low_x, high_x = radius, big - radius
    low_y, high_y = NET_BAND[0] * big + radius, NET_BAND[1] * big - radius
    span = high_y - low_y
    dip = max(0.0, min(NET_DIP, 1.0 - NET_CLEARANCE * radius / span)) if span > 0 else 0.0

    upper = []
    for index in range(NET_UPPER):
        t = index / max(1, NET_UPPER - 1)
        sag = 1.0 - (2.0 * t - 1.0) ** 2  # level at the edges, deepest under the tip
        upper.append((low_x + t * (high_x - low_x), low_y + sag * dip * span))
    lower = [
        (low_x + (index + 0.5) / NET_LOWER * (high_x - low_x), high_y)
        for index in range(NET_LOWER)
    ]
    return upper, lower, radius


def icon_image(colors: list[tuple[int, int, int]], size: int = 64) -> Image.Image:
    """The mark, with one server's state on each of its two elements.

    The icon is asked exactly one question - which of my two servers is up - and
    it still answers it, on the two parts the network already has:

    * **the nodes** are the decode server on :5001, because they are the units
      that produce, and because they are what survives the downsample to 16 px:
      the server you actually watch gets the channel you can actually read;
    * **the edges** are the embedder on :5002, because an embedding is the
      relation between things rather than a thing, and because it is the one that
      stays resident.

    So half-up, the normal overnight state, is legible: grey nodes strung on lit
    edges is the router asleep, and nothing else looks like it.
    """
    scale = SUPERSAMPLE
    big = size * scale
    image = Image.new("RGBA", (big, big), (0, 0, 0, 0))
    draw = ImageDraw.Draw(image, "RGBA")

    node_color = colors[0] if colors else DOWN
    edge_color = colors[1] if len(colors) > 1 else node_color

    def at(x: float, y: float) -> tuple[float, float]:
        return (x * big, y * big)

    apex = at(0.5, MARK_APEX_Y)
    left = at(0.5 - MARK_HALF_W, MARK_EQUATOR_Y)
    right = at(0.5 + MARK_HALF_W, MARK_EQUATOR_Y)
    near = at(0.5, MARK_NEAR_Y)
    base = at(0.5, MARK_BASE_Y)

    upper, lower, radius = net_layout(big)
    width = max(1, round(NET_EDGE_W * big))
    for a in lower:
        for b in upper:
            if abs(a[0] - b[0]) < NET_REACH * big:
                draw.line([a, b], fill=(*edge_color, 185), width=width)
    for a, b in zip(upper, upper[1:], strict=False):  # pairs, so the last node has no partner
        draw.line([a, b], fill=(*edge_color, 150), width=width)
    for point in upper + lower:
        draw.ellipse(
            (point[0] - radius, point[1] - radius, point[0] + radius, point[1] + radius),
            fill=(*node_color, 255),
        )

    # The solid last, opaque, so it covers the far half of the network - the same
    # ordering the SVG uses, minus the near strands it restores on top, which are a
    # pixel wide here and would only muddy the silhouette.
    for triangle, color in (
        ((apex, right, near), MARK_LIT_UPPER),
        ((apex, left, near), MARK_DARK_UPPER),
        ((base, right, near), MARK_LIT_LOWER),
        ((base, left, near), MARK_DARK_LOWER),
    ):
        draw.polygon(list(triangle), fill=(*_legible(color), 255))

    return image.resize((size, size), Image.Resampling.LANCZOS)


# Every size the shell asks for: 16 and 32 for the taskbar and Explorer's small
# views, 48 for the desktop, 256 for the extra-large view and the Alt-Tab card.
ICON_FILE_SIZES = (16, 24, 32, 48, 64, 128, 256)


def write_icon_file(path: Path, colors: list[tuple[int, int, int]]) -> Path:
    """Write the mark as a Windows .ico, at every size the shell asks for.

    One function, three consumers - this window's icon, the desktop shortcut's,
    and `graphics/warden.ico` in the repo - so the tray light and the
    taskbar button are the same picture by construction rather than by two
    implementations agreeing.

    Each size is rendered at its own size rather than downscaled from 256: the
    mark is drawn at 4x and LANCZOS-resampled per size, and a 16 px tile resampled
    once from its own 64 px render is sharper than one resampled twice. Pillow
    would otherwise write the whole file from a single image plus `sizes`, which
    is what `append_images` overrides.
    """
    frames = [icon_image(colors, size=size) for size in ICON_FILE_SIZES]
    frames[-1].save(
        path,
        format="ICO",
        sizes=[(size, size) for size in ICON_FILE_SIZES],
        append_images=frames[:-1],
    )
    return path


def tooltip(status: dict[str, dict]) -> str:
    """Windows truncates a tray tooltip at 127 characters, so this stays terse:
    port and state per server and nothing else. Detail is one click away."""
    parts = [
        f"{label(name)} :{row['port']} {'up' if row.get('listening') else 'down'}"
        for name, row in status.items()
    ]
    return "llama-warden - " + ", ".join(parts) if parts else "llama-warden"


# --- the text UI -------------------------------------------------------------------

WARDEN_THEME = Theme(
    name="warden",
    background=BG,
    surface=SURFACE,
    panel=BORDER,
    foreground=TEXT,
    primary=ACCENT,
    secondary=MUTED,
    accent=ACCENT,
    success=OK_HEX,
    warning=WARN_HEX,
    error=BAD_HEX,
    dark=True,
)


def key_hints(bindings: list[Binding]) -> str:
    """The third status line, derived from BINDINGS rather than typed out beside
    them so a key cannot be added without appearing here.

    It exists because `Footer` does not survive this window. Textual docks the
    footer to the last row, and under conhost the last row is the one thing you
    cannot count on seeing: the window height is not a whole multiple of the cell
    height, so it is clipped. `Footer` additionally defers its first real compose
    until the screen reports a binding change while the app has focus, and a
    classic console does not send focus events at all. Two independent ways for
    the key list to be invisible, against one line of text that always paints.
    """
    return "  ".join(f"[b]{b.key}[/b] {b.description}" for b in bindings if b.show)


class SnappedScrollBarRender(ScrollBarRender):
    """A thumb that is a whole number of cells, and always the same number.

    Textual draws the thumb to an eighth of a cell, using the block glyphs in
    `VERTICAL_BARS` for whatever is left over at each end. A thumb 2.4 cells long
    is therefore two solid cells plus a faint partial one that changes size every
    time the position crosses an eighth - and against 5000 log lines in a 39-row
    pane the leftover IS most of the thumb, so it grows and shrinks at the bottom
    while the top sits still for several mouse ticks. Precision below one cell
    that the reader cannot act on buys nothing and reads as a fault.

    The quantisation happens here and Textual's own renderer still does the
    drawing, handed a geometry scaled by `CELL` so every value it derives lands
    on a cell boundary: `thumb_size` whole, and `position` a whole multiple of
    `len(VERTICAL_BARS)`. Its two partial-glyph branches then look up `" "` and
    skip themselves, so no glyph list is overridden and no segment, style or
    mouse-target is reimplemented here.
    """

    CELL: ClassVar[int] = 8

    @classmethod
    def render_bar(
        cls,
        size: int = 25,
        virtual_size: float = 50,
        window_size: float = 20,
        position: float = 0,
        **kwargs,
    ) -> Segments:
        thumb = size
        if virtual_size:
            thumb = min(size, max(1, round(size * window_size / virtual_size)))
        travel = size - thumb
        span = virtual_size - window_size
        if travel <= 0 or span <= 0:
            # Nothing to scroll, or the thumb fills the track. Left alone rather
            # than special-cased: super's own geometry is exact in that case, and
            # a scaled `window_size` equal to `virtual_size` would divide by zero.
            return super().render_bar(
                size=size,
                virtual_size=virtual_size,
                window_size=window_size,
                position=position,
                **kwargs,
            )
        offset = min(travel, max(0, round(position / span * travel)))
        return super().render_bar(
            size=size,
            virtual_size=size * cls.CELL,
            window_size=thumb * cls.CELL,
            position=offset * cls.CELL,
            **kwargs,
        )


# Every scrollbar in this app, which is the two log panes. `renderer` is the
# extension point Textual documents on ScrollBar for exactly this.
ScrollBar.renderer = SnappedScrollBarRender


class WindowButton(Static):
    """One cell of window chrome. A `Static` and not a `Button` because a Button
    is three rows tall with a border, and this row is one row tall by design."""

    def __init__(self, glyph: str, action: str, tip: str) -> None:
        super().__init__(glyph, classes="win-btn", id=f"win-{action}")
        self.action_name = action
        self.tooltip = tip

    def on_click(self) -> None:
        getattr(self.app, f"action_{self.action_name}")()

    def on_mouse_down(self, event: events.MouseDown) -> None:
        # Stops here rather than bubbling to TitleBar, which would otherwise read
        # a press on the close button as the start of a window drag.
        event.stop()


class TitleBar(Horizontal):
    """The window's own title bar, drawn by the app that owns the window.

    It exists because the host's caption is gone (`frameless_style`), and it has
    to carry everything that went with it: the name, the controls, and somewhere
    to grab. The controls are hide and quit and not minimize/maximize/close -
    those are Windows' three, and only two of them mean anything here. Close is
    quit, because a window that closes without stopping the backend would be the
    close button `disable_console_close` deleted, wearing a different hat.

    `◆` is the mark at one character: the obelisk's silhouette is a diamond, so
    the smallest possible drawing of it is one that already exists in the font.
    """

    DEFAULT_CSS = """
    TitleBar { height: 1; background: $surface; }
    TitleBar #title-text { width: 1fr; padding: 0 1; color: $foreground; }
    TitleBar .win-btn { width: 5; content-align: center middle; color: $secondary; }
    TitleBar .win-btn:hover { background: $panel; color: $foreground; }
    """

    def __init__(self, title: str, draggable: bool) -> None:
        super().__init__()
        self.title_text = title
        self.draggable = draggable
        self._drag: tuple[tuple[int, int], tuple[int, int]] | None = None

    def compose(self) -> ComposeResult:
        yield Static(f"[b]◆[/b] {self.title_text}", id="title-text")
        yield WindowButton("─", "hide", "hide to tray")
        yield WindowButton("✕", "quit_agent", "stop the agent and quit")

    # --- dragging ------------------------------------------------------------------
    #
    # Tracked in screen pixels through `GetCursorPos`, not in the cell coordinates
    # the mouse event carries: a window dragged by cell moves in ~8 px steps and
    # can never be put down where the pointer actually is. The offset is captured
    # once on press and applied absolutely, so a dropped intermediate move event
    # (which a terminal will do under load) leaves no accumulated drift.

    def on_mouse_down(self, event: events.MouseDown) -> None:
        if not self.draggable:
            return
        self._drag = (cursor_position(), console_window_origin())
        self.capture_mouse()

    def on_mouse_move(self, event: events.MouseMove) -> None:
        if self._drag is None:
            return
        (start_x, start_y), (win_x, win_y) = self._drag
        now_x, now_y = cursor_position()
        move_console_window(win_x + now_x - start_x, win_y + now_y - start_y)

    def on_mouse_up(self, event: events.MouseUp) -> None:
        if self._drag is not None:
            self._drag = None
            self.release_mouse()


class AgentConsole(App):
    """Status, both server logs and the agent's own, in one window.

    Every call into the warden is a threaded worker. `server_status` shells out
    to PowerShell (~885 ms) and `start` can legitimately block for a minute
    waiting on ports; either on the UI thread would freeze the display for
    exactly as long as the thing you were watching took. `verdict` is the
    exception and is cheap on purpose: it reads the last completed tick instead
    of measuring again (0001).
    """

    TITLE = "llama-warden"
    # Every height here is 1fr for one reason: TabPane and ContentSwitcher size to
    # their content by default, so a pane holding 400 log lines makes the SCREEN
    # taller than the window. Textual then grows a second scrollbar outside the
    # first and scrolls the whole app, which silently pushes #status off the top
    # (observed live 2026-08-15). The log is the only thing that may scroll, so it
    # is the only thing allowed to exceed its box.
    #
    # Backgrounds are set explicitly rather than left to the theme because a
    # Textual widget with no background shows the one behind it, and the point of
    # the OLED palette is that black is actually painted.
    CSS = """
    Screen { overflow: hidden; background: $background; color: $foreground; }
    #status { height: 3; padding: 0 1; background: $surface; }
    TabbedContent { height: 1fr; }
    TabbedContent ContentSwitcher { height: 1fr; background: $background; }
    Tabs { background: $surface; }
    TabPane { height: 1fr; padding: 0 1; background: $background; }
    RichLog { height: 1fr; background: $background; scrollbar-size-vertical: 1; }
    """
    BINDINGS = [
        Binding("s", "start", "start"),
        Binding("x", "stop", "stop"),
        Binding("r", "restart", "restart"),
        Binding("l", "logs_folder", "logs"),
        Binding("h", "hide", "hide"),
        Binding("q", "quit_agent", "quit"),
    ]

    # Poll intervals, chosen against the measured cost of each probe rather than
    # against what feels responsive: status is a subprocess and the logs are an
    # ordinary bounded file read. The verdict is a dictionary the watch thread
    # already built, so its interval is about how stale the line on screen may
    # look, not about what asking for it costs.
    STATUS_SECONDS = 5.0
    VERDICT_SECONDS = 3.0
    LOG_SECONDS = 1.0

    # How often the window is asked what state it is in. Cheap (`IsIconic` and
    # `IsWindowVisible` are flag reads), and it is the response to a click, so it
    # has to be well under the ~250 ms at which a person notices a lag.
    WINDOW_SECONDS = 0.2

    def __init__(
        self,
        api: AgentAPI,
        on_quit: Callable[[], None] | None = None,
        *,
        owns_console: bool = False,
    ) -> None:
        super().__init__()
        self.api = api
        self.on_quit = on_quit
        # The same licence the win32 calls run under: false means this window is
        # somebody's terminal that we are a guest in, so nothing here reshapes it,
        # moves it, or makes it vanish when they minimize it. Default false so a
        # test never reaches for a console it does not have.
        self.owns_console = owns_console
        self._offsets: dict[str, int | None] = dict.fromkeys(api.servers)
        self._records_seen = 0
        self._status: dict[str, dict] = {}
        self._watch: dict = {}
        self._tray: TrayIcon | None = None

    def compose(self) -> ComposeResult:
        yield TitleBar(self.TITLE, draggable=self.owns_console)
        yield Static("starting...", id="status")
        with TabbedContent():
            for name in self.api.servers:
                with TabPane(label(name), id=f"tab-{name}"):
                    yield RichLog(id=f"log-{name}", wrap=False, markup=False, max_lines=5000)
            with TabPane("agent", id="tab-agent"):
                yield RichLog(id="log-agent", wrap=False, markup=False, max_lines=2000)

    def on_mount(self) -> None:
        self.register_theme(WARDEN_THEME)
        self.theme = "warden"
        self.set_interval(self.STATUS_SECONDS, self.poll_status)
        self.set_interval(self.VERDICT_SECONDS, self.poll_verdict)
        self.set_interval(self.LOG_SECONDS, self.poll_logs)
        # Unconditional: a guest console is never minimized-to-tray by us, but it
        # can still be shown and hidden from the tray menu, which is the other
        # half of what this tick reports.
        self.set_interval(self.WINDOW_SECONDS, self.watch_window)
        self.poll_status()
        self.poll_verdict()
        self.poll_logs()

    def watch_window(self) -> None:
        """The whole window-state tick: minimize means hide to tray, and the tray
        menu is told whenever the window appears or disappears by any route.

        A window procedure could answer `WM_SYSCOMMAND`/`SC_MINIMIZE` directly,
        but this window belongs to conhost.exe: `SetWindowLongPtr(GWL_WNDPROC)`
        across a process boundary is exactly what Windows refuses. So the state is
        read instead of intercepted. The cost is that the minimize animation plays
        before the window disappears, which is what every console-hosted tray app
        does and is not worth a second process to avoid.

        The refresh lives here rather than beside each `hide_console()` call for
        the same reason `owns_console` is checked HERE and not only where the poll
        is scheduled: a rule enforced at the call site is one the next caller does
        not inherit. Polling the answer covers the minimize, the title bar button,
        the `h` key and anything added later, without their authors knowing.
        """
        if self.owns_console and console_is_minimized():
            hide_console()
        if self._tray is not None:
            self._tray.refresh_menu()

    def on_resize(self, event: events.Resize) -> None:
        """Conhost recalculates its scrollbar whenever the frame changes, so the
        buffer is re-matched on every resize rather than only at startup. The call
        is a no-op once they agree, which is why it can run on a hot path."""
        if self.owns_console:
            match_console_buffer()

    # --- polling -------------------------------------------------------------------

    @work(thread=True, exclusive=True, group="status")
    def poll_status(self) -> None:
        status = self.api.server_status()
        self.call_from_thread(self._apply_status, status)

    def poll_verdict(self) -> None:
        """Read what the watch thread last decided. No worker, no thread.

        It is a dictionary lookup, so a thread would cost more than it saves and
        would hand the UI a race in exchange for nothing.
        """
        try:
            self._watch = self.api.verdict()
        except Exception as exc:  # noqa: BLE001 - a dead sensor must not kill the UI
            log.debug("verdict unavailable: %s", exc)
            self._watch = {}
        self._render_status()

    @work(thread=True, exclusive=True, group="logs")
    def poll_logs(self) -> None:
        """Delta reads, the same mechanism the admin log pane uses (0034).

        `reset` means the offset is meaningless - the launcher truncates each log
        on start, so an offset held across a restart points into the middle of a
        different file. The pane clears rather than splicing one server's output
        onto another's.
        """
        for name in self.api.servers:
            try:
                payload = self.api.read_log(name, 400, self._offsets[name])
            except Exception as exc:  # noqa: BLE001
                log.debug("log read failed for %s: %s", name, exc)
                continue
            self._offsets[name] = payload.get("next_offset")
            lines = payload.get("lines") or []
            if payload.get("reset") or lines:
                self.call_from_thread(
                    self._append_log, name, lines, bool(payload.get("reset"))
                )
        pending = list(self.api.records)[self._records_seen :]
        if pending:
            self._records_seen += len(pending)
            self.call_from_thread(self._append_log, "agent", pending, False)

    # --- rendering -----------------------------------------------------------------

    def _apply_status(self, status: dict[str, dict]) -> None:
        self._status = status
        self._render_status()
        if self._tray is not None:
            self._tray.update(status)

    def _render_status(self) -> None:
        chips = []
        for name, row in self._status.items():
            up = row.get("listening")
            pid = f" pid {row['pid']}" if row.get("pid") else ""
            color = "green" if up else "grey62"
            chips.append(
                f"[{color}]{label(name)} :{row['port']} {'up' if up else 'down'}{pid}[/]"
            )
        res = self._watch.get("resources") or {}
        vram = (
            f"VRAM {res['vram_used_mb'] / 1024:.1f}/{res['vram_total_mb'] / 1024:.1f} GB"
            if res.get("vram_total_mb")
            else "VRAM -"
        )
        games = ", ".join(res.get("games_running") or []) or "none"
        second = (
            f"[grey62]GPU {percent(res.get('gpu_percent'))}  "
            f"foreign {percent(res.get('foreign_gpu_percent'))}  {vram}  games: {games}[/]"
        )
        self.query_one("#status", Static).update(
            "\n".join(
                ["  ".join(chips), second, self._verdict_line(), key_hints(self.BINDINGS)]
            )
        )

    def _verdict_line(self) -> str:
        """Why the consumers are doing what they are doing, in one line.

        The reason text comes from `policy.is_contended` and reaches here
        unchanged, because a pipeline that stopped on its own has to be able to
        say what it saw. A verdict with nobody to tell is still shown: a warden
        configured with no consumers is a valid way to run one, and a line
        reading "nobody to tell" is how that is visible rather than mysterious.
        """
        verdict = self._watch.get("verdict") or {}
        if not verdict:
            return "[grey62]no verdict yet[/]"
        color, word = ("yellow", "YIELDED") if verdict.get("yielded") else ("green", "running")
        parts = [f"[{color}]{word}[/] [grey62]{verdict.get('reason', '')}[/]"]
        if not self._watch.get("enabled", True):
            parts.append("[grey62](policy off)[/]")
        heard = [
            f"{name}: {row.get('action') or '-'}" + ("!" if row.get("error") else "")
            for name, row in (self._watch.get("consumers") or {}).items()
        ]
        parts.append(f"[grey62]{', '.join(heard) if heard else 'nobody to tell'}[/]")
        if self._watch.get("probe_error"):
            parts.append(f"[red]probe: {self._watch['probe_error']}[/]")
        return "  ".join(parts)

    def _append_log(self, name: str, lines: list[str], reset: bool) -> None:
        """Follow the tail only while the pane is already at the tail.

        `RichLog.auto_scroll` is unconditional - every `write` calls `scroll_end`
        - so a batch arriving while the reader is scrolled up yanks them back to
        the bottom, and because the buffer is still filling toward `max_lines`
        the thumb they were aiming at shrinks under the pointer as it goes. The
        decision is taken once per batch, BEFORE the first write, since the first
        write is what would have moved the pane. A `reset` cleared the pane, so
        it is at the end by definition and the tail is followed.

        The final scroll is `immediate` because `write`'s own is not: it defers
        through `call_after_refresh`, so the pane's offset still reads as the old
        one until a frame has been drawn. The *next* batch asks that offset
        whether to follow, and a batch arriving before the refresh would read a
        pane that had not caught up yet and conclude the reader had scrolled away
        - after which the log silently stops following. Writing with
        `scroll_end=follow` as well is not redundant: before the pane's size is
        known, `write` queues the render and replays it with that flag later, and
        an immediate scroll over an empty widget moves nothing.
        """
        pane = self.query_one(f"#log-{name}", RichLog)
        if reset:
            pane.clear()
        follow = pane.is_vertical_scroll_end
        for line in lines:
            pane.write(line, scroll_end=follow)
        if follow and lines:
            pane.scroll_end(animate=False, immediate=True)

    # --- actions -------------------------------------------------------------------

    @work(thread=True, exclusive=True, group="lifecycle")
    def _lifecycle(self, verb: str) -> None:
        """Serialized into one worker group because the agent's own lock would
        block the second caller anyway, and a queued keystroke is friendlier than
        a UI that appears hung while it waits on a lock it cannot see."""
        action = {"start": self.api.start, "stop": self.api.stop, "restart": self.api.restart}
        try:
            log.info("%s requested from the console", verb)
            action[verb]()
        except Exception as exc:  # noqa: BLE001 - HTTPException included; it is a UI, not a route
            log.warning("%s failed: %s", verb, getattr(exc, "detail", exc))
        self.call_from_thread(self.poll_status)

    def action_start(self) -> None:
        self._lifecycle("start")

    def action_stop(self) -> None:
        self._lifecycle("stop")

    def action_restart(self) -> None:
        self._lifecycle("restart")

    def action_logs_folder(self) -> None:
        if hasattr(os, "startfile"):
            os.startfile(self.api.log_dir)  # noqa: S606 - a directory we own, on the host

    def action_hide(self) -> None:
        hide_console()

    def action_quit_agent(self) -> None:
        """The ONE way the agent shuts down. The tray's Quit item routes here too.

        There were two, and they were not the same: this one forgot
        `icon.stop()`, so quitting with `q` or the close glyph took the server
        down and left the notification-area icon behind - a tray icon for a
        process that no longer exists, which Windows only clears when the mouse
        happens to pass over it. Stopping the icon is not optional cleanup, it is
        half of what quitting means, so both entry points share one body.

        `Icon.stop()` posts WM_STOP to the icon's own hidden window, so it is safe
        from this thread; the tray thread's message pump does the rest.
        """
        if self.on_quit is not None:
            self.on_quit()
        if self._tray is not None:
            self._tray.icon.stop()
        self.exit()


# --- tray --------------------------------------------------------------------------


class TrayIcon:
    """pystray wrapper that knows how to reach the app and nothing else.

    Runs on its own daemon thread. pystray's Windows backend is a hidden window
    plus a message pump, which is happy off the main thread; the macOS
    restriction that makes `run_detached` necessary elsewhere does not apply.
    """

    def __init__(self, api: AgentAPI, app: AgentConsole) -> None:
        self.api = api
        self.app = app
        self._status: dict[str, dict] = {}
        self._menu_state: tuple[bool, bool, bool] | None = None
        enable_dark_menus()
        # `visible` takes a callable, evaluated each time the menu is opened, so
        # an item that cannot do anything is not there to be clicked. The rule
        # for the servers is deliberately not "up or down": there are two of
        # them, and embed alive while the router sleeps is the NORMAL overnight
        # state (0023). Hiding Start whenever anything at all is listening would
        # leave no way to start the router from the tray on exactly the night it
        # is wanted, so Start asks whether anything is still down and Stop asks
        # whether anything is still up. Both showing at once is not a
        # contradiction; it is the half-up state, stated.
        self.icon = pystray.Icon(
            "llama-warden",
            icon_image(icon_colors({}, list(api.servers))),
            "llama-warden",
            menu=pystray.Menu(
                # Still `default` while invisible: pystray dispatches a
                # double-click through the unfiltered item list, so the icon's
                # double-click keeps showing (and focusing) the window either way.
                pystray.MenuItem(
                    "Show console", self._show, default=True,
                    visible=lambda _: not console_is_visible(),
                ),
                pystray.MenuItem(
                    "Hide console", self._hide, visible=lambda _: console_is_visible()
                ),
                pystray.Menu.SEPARATOR,
                pystray.MenuItem(
                    "Start servers", self._act("start"), visible=lambda _: self.any_down()
                ),
                pystray.MenuItem(
                    "Stop servers", self._act("stop"), visible=lambda _: self.any_up()
                ),
                pystray.MenuItem(
                    "Restart servers", self._act("restart"), visible=lambda _: self.any_up()
                ),
                pystray.Menu.SEPARATOR,
                pystray.MenuItem("Open logs folder", self._logs),
                pystray.MenuItem("Quit agent", self._quit),
            ),
        )

    def any_up(self) -> bool:
        return any(row.get("listening") for row in self._status.values())

    def any_down(self) -> bool:
        """True before the first status arrives, too: an empty status is not
        evidence that everything is running, and Start is the safe item to offer
        while nothing is known - it is idempotent, and Stop is not."""
        return not self._status or not all(
            row.get("listening") for row in self._status.values()
        )

    def start(self) -> None:
        threading.Thread(target=self.icon.run, name="tray", daemon=True).start()

    def refresh_menu(self) -> None:
        """Re-ask the `visible` callables, which nothing else ever does.

        pystray's win32 backend builds one HMENU and hands the same handle to
        every right-click; `_update_menu` is the only thing that rebuilds it, and
        only `Icon.update_menu()` calls it. Without this the menu is frozen at
        the state it was constructed in - which is the console hidden and no
        status polled yet, so it offered Show and Start forever.

        Cheap to call on a timer: the answers are compared first, and DestroyMenu
        plus AppendMenu only happen when one of them actually moved.
        """
        state = (console_is_visible(), self.any_up(), self.any_down())
        if state == self._menu_state:
            return
        self._menu_state = state
        self.icon.update_menu()

    def update(self, status: dict[str, dict]) -> None:
        self._status = status
        self.icon.icon = icon_image(icon_colors(status, list(self.api.servers)))
        self.icon.title = tooltip(status)
        self.refresh_menu()

    # pystray hands every callback `(icon, item)`.
    def _show(self, *_: object) -> None:
        show_console()
        self.refresh_menu()

    def _hide(self, *_: object) -> None:
        hide_console()
        self.refresh_menu()

    def _logs(self, *_: object) -> None:
        if hasattr(os, "startfile"):
            os.startfile(self.api.log_dir)  # noqa: S606

    def _act(self, verb: str) -> Callable[..., None]:
        def run(*_: object) -> None:
            # Straight onto the app's lifecycle worker, so a tray click and a
            # keystroke take the identical path and cannot race each other.
            self.app.call_from_thread(getattr(self.app, f"action_{verb}"))

        return run

    def _quit(self, *_: object) -> None:
        # Onto the app's own quit action, exactly as `_act` does for the lifecycle
        # verbs: a tray click and a keystroke take the identical path, so neither
        # can grow a step the other lacks.
        self.app.call_from_thread(self.app.action_quit_agent)


def adopt_console_window() -> None:
    """Turn a console host's window into this program's window.

    Called once, and only where `disable_console_close` is: all four calls are
    destructive to a window we would merely be borrowing otherwise.

    The buffer is matched twice on purpose. Dropping the caption gives the client
    area back the rows the title bar occupied, so conhost resizes its console
    immediately after the frame change - and a buffer matched only before that
    would be one row short of the window, which is a scrollbar again.

    A failure here is cosmetic by definition, so it is logged and stepped over:
    an agent that refused to start because it could not draw its own icon would
    have traded the whole service for a picture.
    """
    match_console_buffer()
    strip_console_frame()
    match_console_buffer()
    try:
        # Both servers drawn as up, like the .ico and the shortcut: a taskbar
        # button is an identity. The status light is the tray icon, which is a
        # different picture updated on a different clock.
        path = Path(tempfile.gettempdir()) / "llama-warden-window.ico"
        write_icon_file(path, [OK, OK])
        if not set_console_icon(path):
            log.debug("window icon not applied; taskbar keeps the host's own")
        # Both halves or neither: the icon alone leaves the taskbar button on
        # pwsh's picture, and the identity alone makes an anonymous window its own
        # app. See `set_taskbar_identity` for why this has to happen while the
        # window is hidden, which is the state `run` leaves it in next.
        if not set_taskbar_identity():
            log.debug("taskbar identity not applied; the button stays grouped")
    except Exception as exc:  # noqa: BLE001 - a picture must not stop the agent
        log.debug("window icon failed: %s", exc)


def run(api: AgentAPI, *, owns_console: bool, on_quit: Callable[[], None]) -> None:
    """Blocking. Owns the main thread for as long as the agent lives.

    `owns_console` is the whole safety story for the win32 calls: false means we
    are a guest in somebody's terminal, so the window is left alone apart from
    its title.
    """
    set_console_title("llama.cpp agent")
    app = AgentConsole(api, on_quit=on_quit, owns_console=owns_console)
    tray = TrayIcon(api, app)
    app._tray = tray
    tray.start()
    if owns_console:
        disable_console_close()
        adopt_console_window()
        hide_console()
    app.run()
