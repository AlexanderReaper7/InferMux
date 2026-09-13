#!/usr/bin/env python3
"""Regenerate the warden's Windows icon from the mark the tray already draws.

    uv run tools/build_ico.py

Writes `graphics/warden.ico`, which is what the desktop shortcut wears.

It calls `warden.console.write_icon_file` rather than drawing anything, so the
shortcut, the taskbar and the tray are one picture by construction rather than by
two implementations agreeing - the warden writes its own window icon with the
same call at startup. Both servers are passed as up: a shortcut is an identity,
not a status light, and one whose icon depended on when it was last generated
would be a lie the moment the backend stopped.
"""

from pathlib import Path

from warden import console

OUT = Path(__file__).resolve().parents[1] / "graphics" / "warden.ico"


def main() -> None:
    colors = console.icon_colors(
        {"router": {"listening": True}, "embed": {"listening": True}}, ["router", "embed"]
    )
    console.write_icon_file(OUT, colors)
    print(f"wrote {OUT} ({OUT.stat().st_size} bytes, {len(console.ICON_FILE_SIZES)} sizes)")


if __name__ == "__main__":
    main()
