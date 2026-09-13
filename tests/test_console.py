"""The warden's tray icon and text UI (`warden/console.py`).

What is worth testing here is narrow and specific. Not that Textual draws (it
does), but that the things a person reads at a glance are right: the colour of
the tray light, whether a log pane splices one server's output onto another's
after a restart, and that the UI measures nothing itself. Each has exactly one
correct answer, and none is visible in a screenshot taken at the wrong moment.
"""

from collections import deque
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

import pytest
from textual.widgets import Footer, Header, TabbedContent

from warden import console as console_module


@pytest.fixture
def console():
    return console_module


class FakeAgent:
    """Seven callables and three attributes: the whole `AgentAPI`, which is the
    point of it being a dataclass of callables rather than the module."""

    def __init__(self) -> None:
        self.servers = {"router": 5001, "embed": 5002}
        self.status = {
            "router": {"port": 5001, "listening": True, "pid": 42},
            "embed": {"port": 5002, "listening": False, "pid": None},
        }
        self.log_payloads: dict[str, list[dict]] = {}
        self.calls: list[str] = []
        self.requested: list[str] = []
        self.records: deque[str] = deque(maxlen=100)
        # Queued watcher dictionaries, for tests that need the verdict to change.
        self.verdicts: list[dict] = []

    def read_log(self, which, tail=200, since=None):
        self.requested.append(which)
        queue = self.log_payloads.get(which) or []
        return queue.pop(0) if queue else {"lines": [], "next_offset": since or 0}

    # What `watch.Watcher.as_dict()` returns: the measurement AND what was
    # decided from it. The console is handed both and measures neither.
    WATCH = {
        "enabled": True,
        "verdict": {
            "yielded": False,
            "action": "resume",
            "reason": "GPU is free",
            "since": None,
        },
        "consumers": {"episteme": {"action": "resume", "age_seconds": 4.0, "error": None}},
        "probe_error": None,
        "resources": {
            "gpu_percent": 4,
            "foreign_gpu_percent": 0.0,
            "vram_used_mb": 8294,
            "vram_total_mb": 10240,
            "games_running": [],
        },
    }

    def api(self, console):
        return console.AgentAPI(
            server_status=lambda: self.status,
            verdict=lambda: self.verdicts.pop(0) if self.verdicts else self.WATCH,
            read_log=self.read_log,
            start=lambda: self.calls.append("start"),
            stop=lambda: self.calls.append("stop"),
            restart=lambda: self.calls.append("restart"),
            servers=self.servers,
            log_dir=Path("."),
            records=self.records,
        )


# --- the tray light ----------------------------------------------------------------


def test_each_server_gets_its_own_colour(console):
    """The icon answers one question - which of my two servers is up - and a
    single overall colour cannot answer it. Half-up is the interesting state:
    embed alive while the router sleeps is normal overnight."""
    status = {"router": {"listening": True}, "embed": {"listening": False}}
    assert console.icon_colors(status, ["router", "embed"]) == [console.OK, console.DOWN]


def test_an_unreachable_backend_is_grey_not_red(console):
    """A stopped backend is the normal overnight state, not a fault. An icon that
    shows an error every night is an icon nobody reads by the second week."""
    assert console.icon_colors({}, ["router", "embed"]) == [console.DOWN] * 2
    assert console.DOWN != console.BAD


def test_each_server_moves_pixels_on_its_own(console):
    """The mapping the docstring promises - nodes are the router, edges are the
    embedder - only holds if each one is actually visible on its own. Asserting
    the property rather than a colour: a named colour can survive the LANCZOS
    downsample as a blend nobody can point at, where "hold one server fixed and
    the picture still changes" cannot be passed by an invisible channel."""
    both = console.icon_image([console.OK, console.OK], size=32).tobytes()
    router_down = console.icon_image([console.DOWN, console.OK], size=32).tobytes()
    embed_down = console.icon_image([console.OK, console.DOWN], size=32).tobytes()
    assert router_down != both  # the nodes carry :5001
    assert embed_down != both  # the edges carry :5002
    assert router_down != embed_down  # and the two states are not the same picture


def test_no_node_can_leave_the_frame(console):
    """The bounds have the node radius subtracted before anything is placed, so
    this holds for any NET_* the constants are set to - which is the point of
    doing it that way. Tuning coordinates until they happened to fit is what
    produced four clipped arrangements on the first pass."""
    for dip in (0.0, 0.72, 5.0):  # 5.0 is nonsense on purpose
        with mock.patch.object(console, "NET_DIP", dip):
            upper, lower, radius = console.net_layout(256.0)
        for x, y in upper + lower:
            assert radius <= x <= 256.0 - radius
            assert radius <= y <= 256.0 - radius


def test_the_sag_never_reaches_the_rank_below(console):
    """`NET_CLEARANCE` is a clamp, not a hope: the deepest node of the upper rank
    keeps that many radii off the lower rank whatever NET_DIP asks for."""
    with mock.patch.object(console, "NET_DIP", 5.0):
        upper, lower, radius = console.net_layout(256.0)
    gap = min(low[1] for low in lower) - max(high[1] for high in upper)
    assert gap >= console.NET_CLEARANCE * radius - 1e-9


def test_one_server_still_draws(console):
    """`SERVERS` is a dict the agent owns; a future third server or a stripped
    single-server setup must not fall off the end of the colour list."""
    assert console.icon_image([console.OK], size=32).size == (32, 32)
    assert console.icon_image([], size=32).size == (32, 32)


def test_the_tooltip_fits_windows_127_character_limit(console):
    status = {
        "router": {"port": 5001, "listening": True},
        "embed": {"port": 5002, "listening": False},
    }
    text = console.tooltip(status)
    assert "router/main :5001 up" in text and "embed :5002 down" in text
    assert len(text) <= 127


def test_the_tray_menu_only_offers_what_can_be_done(console, monkeypatch):
    """Every item that is showing has to be an action with an effect. Show while
    the window is on screen, or Stop while nothing is listening, is a click that
    does nothing and teaches the reader that the menu does not mean anything.

    The half-up row is the one that matters and the reason the rule is not "up
    or down": embed alive while the router sleeps is the normal overnight state,
    so Start and Stop are BOTH right then - Start still has a router to start."""
    agent = FakeAgent()
    tray = console.TrayIcon(agent.api(console), None)

    def offered(on_screen: bool, status: dict) -> set[str]:
        monkeypatch.setattr(console, "console_is_visible", lambda: on_screen)
        tray.update(status)
        return {item.text for item in tray.icon.menu if not item.text.startswith("-")}

    def row(listening: bool, port: int) -> dict:
        return {"listening": listening, "port": port}

    up = {"router": row(True, 5001), "embed": row(True, 5002)}
    down = {"router": row(False, 5001), "embed": row(False, 5002)}
    half = {"router": row(False, 5001), "embed": row(True, 5002)}

    assert offered(True, up) == {"Hide console", "Stop servers", "Restart servers",
                                 "Open logs folder", "Quit agent"}
    assert offered(False, down) == {"Show console", "Start servers",
                                    "Open logs folder", "Quit agent"}
    assert {"Start servers", "Stop servers", "Restart servers"} <= offered(True, half)
    # Nothing known yet: Start is idempotent, Stop is not, so only Start is offered.
    assert offered(True, {}) & {"Start servers", "Stop servers"} == {"Start servers"}


def test_the_tray_menu_is_rebuilt_when_the_answers_change(console, monkeypatch):
    """The `visible` callables above are only consulted while pystray builds the
    HMENU, and its win32 backend builds ONE and hands the same handle to every
    right-click. Iterating `icon.menu` in a test re-runs them and proves nothing:
    live, the menu froze at construction - console hidden, no status polled - and
    offered Show and Start with both servers running. `update_menu()` is the only
    thing that rebuilds it, so what is asserted is that it is called when an
    answer moves, and not called when nothing has."""
    rebuilds: list[int] = []
    agent = FakeAgent()
    tray = console.TrayIcon(agent.api(console), None)
    monkeypatch.setattr(tray.icon, "update_menu", lambda: rebuilds.append(1))
    monkeypatch.setattr(console, "console_is_visible", lambda: False)

    row = {"listening": True, "port": 5001}
    tray.update({"router": row})
    assert len(rebuilds) == 1  # first status: Start gives way to Stop/Restart
    tray.update({"router": row})
    tray.update({"router": dict(row)})
    assert len(rebuilds) == 1  # same answers, no menu churn on every 5 s poll

    monkeypatch.setattr(console, "console_is_visible", lambda: True)
    tray.refresh_menu()
    assert len(rebuilds) == 2  # shown by any route, not only the tray's own item


async def test_the_window_tick_tells_the_tray_the_window_moved(console, monkeypatch):
    """`hide_console()` is called from the title bar button, the `h` key, the tray
    and the minimize watcher. The refresh is on the tick that reads the window
    state rather than beside each of those, so a path added later is covered
    without its author knowing to add it."""
    refreshed: list[int] = []
    monkeypatch.setattr(console, "console_is_minimized", lambda: False)
    app = console.AgentConsole(FakeAgent().api(console), owns_console=True)
    app._tray = SimpleNamespace(
        refresh_menu=lambda: refreshed.append(1), update=lambda status: None
    )
    async with app.run_test() as pilot:
        await pilot.pause(console.AgentConsole.WINDOW_SECONDS * 2)
    assert refreshed  # the interval is running, not merely defined


def test_the_tray_menu_asks_windows_for_a_dark_one(console, monkeypatch):
    """A `TrackPopupMenu` menu is drawn by the system and comes up light unless
    the process opts in. The opt-in is two undocumented uxtheme ordinals, so what
    is asserted is that it is requested at all - whether Windows honours it is
    Windows' half, and the failure is cosmetic by construction."""
    asked: list[bool] = []
    monkeypatch.setattr(console, "enable_dark_menus", lambda: asked.append(True))
    console.TrayIcon(FakeAgent().api(console), None)
    assert asked == [True]


# --- the window is somebody else's until told otherwise ----------------------------


def test_the_console_is_left_alone_unless_the_agent_owns_it(console, monkeypatch):
    """Run by hand, `GetConsoleWindow()` is the USER'S terminal. Hiding it,
    deleting its close button or tearing the title bar off it would vandalize a
    window we were borrowing, and none of the three comes back.

    Every one of them is patched here rather than only observed, because this test
    runs in a terminal too: `uv run pytest` from a real console would otherwise
    reshape *that* window, which is the bug being guarded against, committed by
    the guard. It passes today only because the harness runs without a console."""
    touched: list[str] = []
    monkeypatch.setattr(console, "disable_console_close", lambda: touched.append("close"))
    monkeypatch.setattr(console, "adopt_console_window", lambda: touched.append("adopt"))
    monkeypatch.setattr(console, "hide_console", lambda: touched.append("hide"))
    monkeypatch.setattr(console, "set_console_title", lambda title: None)

    class DeadApp:
        def __init__(self, *a, **k):
            pass

        def run(self):
            pass

    monkeypatch.setattr(console, "AgentConsole", DeadApp)
    tray = type("T", (), {"start": lambda self: None})
    monkeypatch.setattr(console, "TrayIcon", lambda *a, **k: tray())
    console.run(FakeAgent().api(console), owns_console=False, on_quit=lambda: None)
    assert touched == []
    console.run(FakeAgent().api(console), owns_console=True, on_quit=lambda: None)
    assert touched == ["close", "adopt", "hide"]


def test_the_frame_loses_its_chrome_and_keeps_its_sizing_border(console):
    """What `frameless_style` removes is the whole reason the app draws a title
    row of its own, and what it KEEPS is the part that is easy to lose by
    accident: strip `WS_THICKFRAME` along with the caption and the window becomes
    one nobody can resize, which for a log viewer is worse than the title bar."""
    ws_thickframe, ws_sysmenu, ws_minimizebox = 0x00040000, 0x00080000, 0x00020000
    style = (
        console.WS_CAPTION | console.WS_VSCROLL | console.WS_HSCROLL
        | ws_thickframe | ws_sysmenu | ws_minimizebox
    )
    stripped = console.frameless_style(style)
    assert not stripped & console.WS_CAPTION
    assert not stripped & console.WS_VSCROLL
    assert not stripped & console.WS_HSCROLL
    assert stripped & ws_thickframe  # still resizable
    assert stripped & ws_sysmenu and stripped & ws_minimizebox  # Alt-Space, Win+Down
    assert console.frameless_style(stripped) == stripped  # applying it twice is safe


def test_the_window_is_given_both_an_icon_and_an_identity(console, monkeypatch):
    """The taskbar button is not drawn from the window icon. It is drawn from the
    AppUserModelID the shell resolves for the window, and a console window resolves
    to the process that opened it - so `set_console_icon` alone produced exactly
    what was reported on 2026-08-16: our mark in the title bar, on hover and in
    Alt-Tab, and pwsh's on the taskbar. Neither call is the fix by itself.

    The order is not asserted, but the pairing is: dropping either one leaves a
    window wearing two different pictures at once."""
    done: list[str] = []

    def note(name: str):
        def record(*_: object) -> bool:
            done.append(name)
            return True

        return record

    monkeypatch.setattr(console, "match_console_buffer", note("buffer"))
    monkeypatch.setattr(console, "strip_console_frame", note("frame"))
    monkeypatch.setattr(console, "write_icon_file", lambda path, colors: path)
    monkeypatch.setattr(console, "set_console_icon", note("icon"))
    monkeypatch.setattr(console, "set_taskbar_identity", note("identity"))

    console.adopt_console_window()
    assert "icon" in done and "identity" in done


def test_the_icon_file_carries_every_size_at_its_own_resolution(console, tmp_path):
    """One function writes the window icon, the shortcut's and the repo's, so the
    tray light and the taskbar button cannot become two pictures. The sizes matter
    on their own: Windows picks the nearest and scales the rest, and a 16 px tile
    scaled down from 256 is the mark as a smudge."""
    from PIL import Image

    path = console.write_icon_file(tmp_path / "mark.ico", [console.OK, console.OK])
    with Image.open(path) as img:
        assert set(img.info["sizes"]) == {(s, s) for s in console.ICON_FILE_SIZES}


def test_the_window_is_animated_while_there_is_still_a_window(console, monkeypatch):
    """Order is the whole behaviour. After `SW_HIDE` there is nothing left to
    shrink, and after `SW_RESTORE` the window is already sitting where the
    animation was flying to, so either call in the wrong place is an animation
    nobody sees - and neither raises."""
    order: list[str] = []

    class FakeUser32:
        def ShowWindow(self, hwnd, command):
            order.append(f"show {command}")

        def SetForegroundWindow(self, hwnd):
            order.append("foreground")

    def fake_animate(hwnd, reverse=False):
        order.append("animate back" if reverse else "animate away")
        return True

    monkeypatch.setattr(console, "_user32", FakeUser32())
    monkeypatch.setattr(console, "console_hwnd", lambda: 4242)
    monkeypatch.setattr(console, "animate_to_tray", fake_animate)
    console.hide_console()
    console.show_console()
    assert order == [
        "animate away",
        f"show {console.SW_HIDE}",
        "animate back",
        f"show {console.SW_RESTORE}",
        "foreground",
    ]


def test_a_window_windows_already_minimized_is_not_animated_twice(console, monkeypatch):
    """The minimize path arrives here AFTER Windows has played its own animation
    into the taskbar, and a minimized window's rect is off at -32000. Animating
    then is a wireframe flying out of nowhere."""
    calls: list[str] = []

    class FakeUser32:
        def IsIconic(self, hwnd):
            return 1

        def DrawAnimatedRects(self, *args):
            calls.append("draw")
            return 1

    monkeypatch.setattr(console, "_user32", FakeUser32())
    assert console.animate_to_tray(4242) is False
    assert calls == []


# --- the UI --------------------------------------------------------------------


async def test_status_line_reports_both_servers_and_the_gpu(console):
    agent = FakeAgent()
    app = console.AgentConsole(agent.api(console))
    async with app.run_test() as pilot:
        await app.workers.wait_for_complete()
        await pilot.pause()
        text = str(app.query_one("#status").content)
    assert "router/main :5001 up pid 42" in text
    assert "embed :5002 down" in text
    assert "8.1/10.0 GB" in text
    # The numbers on that line came out of the watcher's dictionary, not out of a
    # probe this process ran.
    assert "GPU 4%" in text


async def test_the_console_shows_the_verdict_and_who_has_heard_it(console):
    """The line a person reads to answer "why has the pipeline stopped". The
    reason text is `policy.is_contended`'s, carried through unchanged, and the
    consumer roll-call is there because a verdict nobody took is the failure this
    design can actually have."""
    agent = FakeAgent()
    agent.verdicts = [
        {
            "enabled": True,
            "verdict": {
                "yielded": True,
                "action": "pause",
                "reason": "foreign GPU load 91% >= 25% (bf6)",
                "since": "2026-09-13T18:00:00+00:00",
            },
            "consumers": {"episteme": {"action": "pause", "age_seconds": 2.0, "error": None}},
            "probe_error": None,
            "resources": {"gpu_percent": 96, "foreign_gpu_percent": 91.0},
        }
    ]
    app = console.AgentConsole(agent.api(console))
    async with app.run_test() as pilot:
        await pilot.pause()
        text = str(app.query_one("#status").content)
    assert "YIELDED" in text
    assert "foreign GPU load 91% >= 25% (bf6)" in text
    assert "episteme: pause" in text


async def test_a_warden_with_nobody_to_tell_says_so(console):
    """Running with no consumers configured is valid - it is what a fresh clone
    does - and has to be legible rather than looking like a delivery failure."""
    agent = FakeAgent()
    agent.verdicts = [dict(FakeAgent.WATCH, consumers={})]
    app = console.AgentConsole(agent.api(console))
    async with app.run_test() as pilot:
        await pilot.pause()
        text = str(app.query_one("#status").content)
    assert "nobody to tell" in text


def test_the_ui_has_no_way_to_measure_anything(console):
    """A probe costs ~3.5s of performance counters and the watch thread is
    already paying it every tick. The seam is the enforcement: with no
    `resources` callable on `AgentAPI` there is nothing here for a later edit to
    call, so the second sweep cannot be reintroduced by accident."""
    fields = set(console.AgentAPI.__dataclass_fields__)
    assert "verdict" in fields
    assert "resources" not in fields


async def test_the_key_hints_are_on_screen_and_derived_from_the_bindings(console):
    """Textual's `Footer` does not survive this window: it docks to the last row,
    which conhost clips when the window height is not a whole multiple of the cell
    height, and its first real compose waits on a binding-change event delivered
    only while the app has focus - which a classic console never reports. So the
    keys live in #status, derived from BINDINGS so the list cannot drift from what
    the keys actually do."""
    agent = FakeAgent()
    app = console.AgentConsole(agent.api(console))
    async with app.run_test() as pilot:
        await app.workers.wait_for_complete()
        await pilot.pause()
        text = str(app.query_one("#status").content)
        assert not app.query(Footer)
    for binding in console.AgentConsole.BINDINGS:
        assert binding.key in text and binding.description in text


async def test_black_is_actually_painted(console):
    """The tokens are Episteme's, inherited at the split, and black is painted
    rather than assumed: a Textual widget with no background shows whatever is
    behind it, which on a terminal is the user's own scheme and not the OLED
    black this asks for."""
    agent = FakeAgent()
    app = console.AgentConsole(agent.api(console))
    async with app.run_test() as pilot:
        await pilot.pause()
        assert app.theme == "warden"
        assert app.current_theme.background == "#000000"
        assert app.current_theme.success == console.OK_HEX
        assert app.screen.styles.background.hex.lower().startswith("#000000")


async def test_the_tab_label_is_display_only(console):
    """`router` is the agent's key: the log file stem, the argument `read_log`
    takes and the `#log-*` id all derive from it. A label change must move only
    what a person reads, or the pane quietly follows a file that does not exist."""
    agent = FakeAgent()
    app = console.AgentConsole(agent.api(console))
    async with app.run_test() as pilot:
        await app.workers.wait_for_complete()
        await pilot.pause()
        tabs = app.query_one(TabbedContent)
        assert tabs.get_tab("tab-router").label_text == "router/main"
        assert app.query_one("#log-router") is not None
    assert agent.requested == ["router", "embed"]


async def test_the_window_controls_do_what_the_keys_do(console, monkeypatch):
    """The title row replaces the caption Windows drew, so it has to carry the
    caption's controls - and they go through the same actions as the keys and the
    tray menu, not to a fourth implementation of hide and quit."""
    hidden: list[str] = []
    quit_called: list[str] = []
    monkeypatch.setattr(console, "hide_console", lambda: hidden.append("hide"))
    agent = FakeAgent()
    app = console.AgentConsole(agent.api(console), on_quit=lambda: quit_called.append("quit"))
    async with app.run_test() as pilot:
        await pilot.pause()
        assert not app.query(Header)  # the caption is drawn once, by us
        assert str(app.query_one("#title-text").content).endswith(console.AgentConsole.TITLE)
        await pilot.click("#win-hide")
        await pilot.pause()
        assert hidden == ["hide"]
        await pilot.click("#win-quit_agent")
        await pilot.pause()
    assert quit_called == ["quit"]


async def test_quitting_takes_the_tray_icon_with_it(console):
    """Live, `q` and the close glyph shut the agent down and left the icon in the
    notification area - a tray icon for a process that no longer exists, which
    Windows only clears when the mouse passes over it. The tray's own Quit item
    did stop it, so there were two quits and only one of them was complete."""
    stopped: list[str] = []
    app = console.AgentConsole(FakeAgent().api(console), on_quit=lambda: None)
    app._tray = SimpleNamespace(
        icon=SimpleNamespace(stop=lambda: stopped.append("stop")),
        refresh_menu=lambda: None,
        update=lambda status: None,
    )
    async with app.run_test() as pilot:
        await pilot.pause()
        app.action_quit_agent()
    assert stopped == ["stop"]


def test_the_tray_quit_item_is_the_apps_quit_action(console):
    """Not a copy of it. The two bodies had already drifted once."""
    called: list[object] = []
    action = object()  # stands in for the app's bound action_quit_agent
    tray = console.TrayIcon(FakeAgent().api(console), None)
    tray.app = SimpleNamespace(call_from_thread=called.append, action_quit_agent=action)
    tray._quit()
    assert called == [action]


def test_a_missing_utilization_reading_is_a_dash_not_none(console):
    """`/resources` always emits `gpu_percent` and its value is null when
    nvidia-smi had no utilization line, so `dict.get(key, "-")` never fires and
    the status bar printed `None%`."""
    assert console.percent(None) == "-"
    assert console.percent(0) == "0%"
    assert console.percent(37.5) == "37.5%"


async def test_a_borrowed_terminal_is_not_dragged_or_hidden_under_the_user(console, monkeypatch):
    """`owns_console` gates the window surgery in `run`; it has to gate the UI
    too. In somebody else's terminal, dragging the title row would move THEIR
    window and the minimize watcher would make it vanish when they minimized it -
    two ways to vandalize a window by clicking on it rather than by starting up."""
    moved: list[tuple[int, int]] = []
    hidden: list[str] = []
    monkeypatch.setattr(console, "move_console_window", lambda x, y: moved.append((x, y)))
    monkeypatch.setattr(console, "console_is_minimized", lambda: True)
    monkeypatch.setattr(console, "hide_console", lambda: hidden.append("hide"))
    agent = FakeAgent()

    app = console.AgentConsole(agent.api(console))  # a guest
    async with app.run_test() as pilot:
        await pilot.pause()
        bar = app.query_one(console.TitleBar)
        assert not bar.draggable
        await pilot.mouse_down(console.TitleBar)
        await pilot.hover(console.TitleBar, offset=(4, 0))
        await pilot.mouse_up(console.TitleBar)
        assert moved == []
        assert app.watch_window() is None and hidden == []

    owner = console.AgentConsole(agent.api(console), owns_console=True)
    async with owner.run_test() as pilot:
        await pilot.pause()
        assert owner.query_one(console.TitleBar).draggable
        owner.watch_window()
    assert hidden == ["hide"]  # minimize means hide to tray, once it is ours


async def test_dragging_the_title_row_moves_the_window_in_screen_pixels(console, monkeypatch):
    """The window has no caption to grab, so the title row is the grab handle.
    It moves the window by the CURSOR's travel, not the mouse event's: the event
    arrives in character cells, and a window that can only be placed on an 8 px
    grid never quite lands where it was dropped."""
    cursor = iter([(100, 100)])
    monkeypatch.setattr(console, "cursor_position", lambda: next(cursor, (140, 130)))
    monkeypatch.setattr(console, "console_window_origin", lambda: (10, 20))
    moved: list[tuple[int, int]] = []
    monkeypatch.setattr(console, "move_console_window", lambda x, y: moved.append((x, y)))
    agent = FakeAgent()
    app = console.AgentConsole(agent.api(console), owns_console=True)
    async with app.run_test() as pilot:
        await pilot.pause()
        await pilot.mouse_down("#title-text")  # bubbles to the row
        await pilot.hover("#title-text", offset=(6, 0))
        await pilot.mouse_up("#title-text")
        assert app.query_one(console.TitleBar)._drag is None  # released on mouse-up
    assert moved and moved[-1] == (10 + 40, 20 + 30)  # origin + cursor delta


async def test_a_press_on_a_window_button_is_not_the_start_of_a_drag(console, monkeypatch):
    """The buttons sit inside the draggable row, so their mouse-down has to stop
    there. Otherwise clicking close also grabs the window, and a click that misses
    by a pixel of travel drags it instead of pressing it."""
    moved: list[tuple[int, int]] = []
    monkeypatch.setattr(console, "move_console_window", lambda x, y: moved.append((x, y)))
    monkeypatch.setattr(console, "cursor_position", lambda: (100, 100))
    monkeypatch.setattr(console, "console_window_origin", lambda: (10, 10))
    agent = FakeAgent()
    app = console.AgentConsole(agent.api(console), owns_console=True)
    async with app.run_test() as pilot:
        await pilot.pause()
        await pilot.mouse_down("#win-hide")
        await pilot.pause()
        assert app.query_one(console.TitleBar)._drag is None
        await pilot.mouse_up("#win-hide")


async def test_a_full_pane_does_not_make_the_whole_app_scroll(console):
    """TabPane and ContentSwitcher size to their content, so 400 log lines made
    the SCREEN taller than the window: Textual grew a second scrollbar outside
    the log's own and scrolled the entire app, and the first row to leave the top
    was #status - the one line the window exists to show. Found live 2026-08-15,
    and invisible to every other test here because they write three lines."""
    agent = FakeAgent()
    agent.log_payloads["router"] = [
        {"lines": [f"line {n}" for n in range(400)], "next_offset": 9}
    ]
    app = console.AgentConsole(agent.api(console))
    async with app.run_test(size=(90, 27)) as pilot:
        await app.workers.wait_for_complete()
        await pilot.pause()
        assert app.screen.virtual_size.height <= app.screen.size.height
        assert app.query_one("#status").region.y == 1


async def test_a_truncated_log_clears_the_pane_instead_of_splicing(console):
    """The launcher truncates each log on start, so an offset held across a
    restart points into the middle of a different file. `read_log` already flags
    that as `reset` (0034); what is tested here is that the pane obeys it, since
    the failure mode is a pane that reads as continuous and is not."""
    agent = FakeAgent()
    agent.log_payloads["router"] = [
        {"lines": ["first boot"], "next_offset": 10, "reset": False},
        {"lines": ["after restart"], "next_offset": 5, "reset": True},
    ]
    app = console.AgentConsole(agent.api(console))
    cleared: list[bool] = []
    async with app.run_test() as pilot:
        await app.workers.wait_for_complete()
        await pilot.pause()
        original = app._append_log
        app._append_log = lambda name, lines, reset: (
            cleared.append(reset),
            original(name, lines, reset),
        )[1]
        app.poll_logs()
        await app.workers.wait_for_complete()
        await pilot.pause()
    assert cleared == [True]
    assert app._offsets["router"] == 5


def test_the_scrollbar_thumb_is_one_size_at_every_position(console):
    """Swept across the whole travel of a full log pane, because the defect only
    appears BETWEEN cell boundaries: Textual's thumb is drawn to an eighth of a
    cell, so at 5000 lines in a 39-row pane it grows and shrinks a partial glyph
    at the bottom as it moves. Two properties, both mechanical: the solid thumb
    never changes length, and no partial block glyph is ever emitted."""
    from textual.scrollbar import ScrollBar

    lengths: set[int] = set()
    partial: list[str] = []
    for position in range(0, 4962, 7):
        segments = ScrollBar.renderer.render_bar(
            size=39, virtual_size=5000, window_size=39, position=position, vertical=True
        ).segments
        assert len(segments) == 39
        lengths.add(sum(1 for s in segments if s.style is not None and s.style.reverse))
        partial += [s.text for s in segments if s.text.strip()]
    assert lengths == {1}, f"thumb changes length across its travel: {sorted(lengths)}"
    assert partial == [], f"partial glyphs still drawn: {set(partial)}"


def test_the_thumb_still_reaches_both_ends(console):
    """The price of snapping is that the thumb must still bottom out, or the
    reader loses the one thing the bar is for: knowing they are at the end."""
    from textual.scrollbar import ScrollBar

    def thumb_at(position: float) -> int:
        segments = ScrollBar.renderer.render_bar(
            size=20, virtual_size=200, window_size=20, position=position, vertical=True
        ).segments
        return next(i for i, s in enumerate(segments) if s.style is not None and s.style.reverse)

    assert thumb_at(0) == 0
    assert thumb_at(180) == 20 - 2  # size - thumb, the last cell it can occupy


async def test_a_log_pane_scrolled_up_is_not_dragged_back_to_the_tail(console):
    """A log arriving under a reader who has scrolled up must leave the view
    where it is. `RichLog(auto_scroll=True)` scrolls to the end on EVERY write,
    which is why `_append_log` passes `scroll_end` explicitly instead. Both
    directions are asserted: pinned to the bottom still has to follow, or the
    pane stops being a live log."""
    agent = FakeAgent()
    app = console.AgentConsole(agent.api(console))
    async with app.run_test(size=(90, 27)) as pilot:
        await app.workers.wait_for_complete()
        await pilot.pause()
        app._append_log("router", [f"line {n}" for n in range(200)], False)
        pane = app.query_one("#log-router")
        # Asserted with NO pause in between, which is the sharper claim: `write`'s
        # own scroll defers to after a refresh, so the offset the *next* batch
        # reads would still be the old one and that batch would decide the reader
        # had scrolled away. Held as a flaky failure of the assertion below until
        # `_append_log` took the last scroll itself, immediately.
        assert pane.is_vertical_scroll_end, "a pane at the tail must follow new lines"
        await pilot.pause()
        assert pane.is_vertical_scroll_end

        pane.scroll_to(y=20, animate=False)
        await pilot.pause()
        parked = pane.scroll_offset.y
        app._append_log("router", ["one more"], False)
        await pilot.pause()
        assert pane.scroll_offset.y == parked

        pane.scroll_end(animate=False)
        await pilot.pause()
        app._append_log("router", ["and another"], False)
        await pilot.pause()
        assert pane.is_vertical_scroll_end


async def test_a_keystroke_and_a_tray_click_take_the_same_path(console):
    """The tray menu calls `action_restart`, not the API, so the worker group
    that serializes lifecycle work cannot be bypassed by clicking instead of
    typing."""
    agent = FakeAgent()
    app = console.AgentConsole(agent.api(console))
    async with app.run_test() as pilot:
        await pilot.press("r")
        await app.workers.wait_for_complete()
        await pilot.pause()
    assert agent.calls == ["restart"]


async def test_the_agents_own_log_lines_reach_the_agent_tab_once(console):
    """`RECORDS` is a deque the agent appends to forever; the pane must consume
    only what it has not already shown, or every poll would repeat the backlog."""
    agent = FakeAgent()
    app = console.AgentConsole(agent.api(console))
    seen: list[list[str]] = []
    async with app.run_test() as pilot:
        await app.workers.wait_for_complete()
        await pilot.pause()
        original = app._append_log
        app._append_log = lambda name, lines, reset: (
            seen.append(list(lines)) if name == "agent" else None,
            original(name, lines, reset),
        )[1]
        agent.records.append("one")
        app.poll_logs()
        await app.workers.wait_for_complete()
        await pilot.pause()
        app.poll_logs()
        await app.workers.wait_for_complete()
        await pilot.pause()
    assert seen == [["one"]]
