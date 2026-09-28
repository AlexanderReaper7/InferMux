"""The process: logging, the HTTP service and the watch loop.

    python -m warden

Logs go to stderr, which is journald under systemd. There is no console and no
tray icon any more; `curl 127.0.0.1:5003/verdict` and
`journalctl -u llama-warden` replace them (0002).
"""

from __future__ import annotations

import argparse
import logging

import uvicorn

from . import agent, watch
from .config import settings
from .probe import NvmlProbe

log = logging.getLogger("warden")


def build_watcher() -> watch.Watcher:
    """Wire the loop to the probe. `watch` never imports `agent`, so this is the
    one place the two meet, which is what keeps each testable on its own."""
    return watch.Watcher(settings, resources=NvmlProbe(settings.our_units))


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--port", type=int, default=settings.port)
    parser.add_argument("--host", default=settings.host)
    args = parser.parse_args()

    # No timestamp: journald stamps every line itself.
    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(name)s: %(message)s")

    watcher = build_watcher()
    watcher.start()
    uvicorn.run(agent.app, host=args.host, port=args.port, log_config=None, log_level="info")


if __name__ == "__main__":
    main()
