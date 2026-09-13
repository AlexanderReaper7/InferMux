"""The process: logging, the HTTP service, the watch loop, and the console.

    uv run python -m warden             # text UI + tray icon, if a console exists
    uv run python -m warden --headless  # the bare service, watching and announcing

`--headless` is not a reduced warden. It measures, decides and announces exactly
the same; there is simply nobody watching it. Every side effect lives in
`agent.py`, `watch.py` and `policy.py`, never in the UI.
"""

from __future__ import annotations

import argparse
import logging
import logging.handlers
import sys
import threading
from collections import deque

import uvicorn

from . import agent, watch
from .config import LOG_DIR, settings

log = logging.getLogger("warden")

# The warden's own log lines, for the console's third tab. A bounded deque rather
# than a file tail because these lines have not been written anywhere yet at the
# moment the UI wants them, and re-reading our own file to display what we just
# logged would be a round trip through the disk for no reason.
RECORDS: deque[str] = deque(maxlen=2000)


class _DequeHandler(logging.Handler):
    """Feeds `RECORDS`, which is the console's `warden` tab."""

    def emit(self, record: logging.LogRecord) -> None:
        RECORDS.append(self.format(record))


def configure_logging(*, to_console: bool) -> None:
    """A rotating file, the deque, and stdout only when nothing is drawing on it.

    A `StreamHandler` under the text UI would scribble log lines over Textual's
    own output and corrupt the display, and uvicorn's default config installs
    exactly that. Hence `log_config=None` at the uvicorn call: root owns the
    handlers, and there is one policy rather than two.

    The file rotates rather than truncating, unlike the llama-server logs the
    launcher manages. Those are megabytes an hour and their value is entirely in
    the present; this one is a few kilobytes a day and its value is mostly in the
    run that crashed, which truncate-on-start is precisely how to lose.
    """
    LOG_DIR.mkdir(parents=True, exist_ok=True)
    formatter = logging.Formatter("%(asctime)s %(levelname)s %(message)s")
    handlers: list[logging.Handler] = [
        logging.handlers.RotatingFileHandler(
            LOG_DIR / "agent.log", maxBytes=2_000_000, backupCount=1, encoding="utf-8"
        ),
        _DequeHandler(),
    ]
    if to_console:
        handlers.append(logging.StreamHandler())
    root = logging.getLogger()
    root.setLevel(logging.INFO)
    for handler in handlers:
        handler.setFormatter(formatter)
        root.addHandler(handler)


def build_watcher() -> watch.Watcher:
    """Wire the loop to the probe. `watch` never imports `agent`, so this is the
    one place the two meet — which is what keeps each testable on its own."""
    return watch.Watcher(settings, resources=agent.resources)


def _console_api(watcher: watch.Watcher):
    """Bind the console's `AgentAPI` to this process's functions.

    Imported here, not at module scope, so `--headless` does not need textual or
    pystray installed to run.
    """
    from . import console

    return console, console.AgentAPI(
        server_status=agent._server_status,
        read_log=agent.read_log,
        start=agent.start_servers,
        stop=agent.stop,
        restart=lambda: agent.restart(None),
        verdict=lambda: watcher.as_dict(),
        servers=agent.SERVERS,
        log_dir=LOG_DIR,
        records=RECORDS,
    )


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--port", type=int, default=settings.port)
    parser.add_argument("--host", default=settings.host)
    parser.add_argument(
        "--headless", action="store_true", help="HTTP service only: no text UI, no tray icon"
    )
    parser.add_argument(
        "--hide",
        action="store_true",
        help=(
            "the console belongs to this process: hide it at startup and remove its close "
            "button. Set by install-task.ps1; never set it when running from a terminal "
            "you want to keep."
        ),
    )
    args = parser.parse_args()

    # The UI needs a console to draw in. Redirected output and a genuinely
    # console-less service both fail this, and both must keep working rather than
    # crashing inside Textual, so the fallback is the behaviour that existed
    # before there was a UI at all.
    interactive = not args.headless and sys.stdout.isatty()
    configure_logging(to_console=not interactive)

    watcher = build_watcher()
    watcher.start()

    if not interactive:
        uvicorn.run(agent.app, host=args.host, port=args.port, log_level="info")
        return

    console, api = _console_api(watcher)
    server = uvicorn.Server(
        uvicorn.Config(agent.app, host=args.host, port=args.port, log_config=None, log_level="info")
    )
    threading.Thread(target=server.run, name="uvicorn", daemon=True).start()
    log.info("Warden listening on http://%s:%d", args.host, args.port)

    def shutdown() -> None:
        watcher.stop()
        server.should_exit = True

    console.run(api, owns_console=args.hide, on_quit=shutdown)


if __name__ == "__main__":
    main()
