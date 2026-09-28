"""The warden's HTTP face, on loopback :5003, no auth.

Read-only since the Linux port (0002). On Windows this was also the actuator:
it launched llama-server, streamed its logs and edited the preset. systemd
runs the servers now, journald has the logs and the preset is in the NixOS
configuration, so what is left is the answer to "what does the warden think,
and what did it see".

Binds 127.0.0.1 only.

Run it through `python -m warden` (see `__main__.py`), never by importing and
serving this module alone: the loop that decides is started there.
"""

from __future__ import annotations

import socket

from fastapi import FastAPI, HTTPException

from . import router, watch
from .config import settings
from .probe import NvmlProbe

app = FastAPI(title="llama-warden")

# Its own probe, so a fresh sweep on request does not move the watcher's
# utilization window.
_probe = NvmlProbe(settings.our_units)


def _port_open(url: str, timeout: float = 0.5) -> bool:
    host, _, port = url.split("://", 1)[-1].split("/", 1)[0].rpartition(":")
    try:
        with socket.create_connection((host or "127.0.0.1", int(port)), timeout=timeout):
            return True
    except (OSError, ValueError):
        return False


@app.get("/verdict")
def verdict() -> dict:
    """What the policy currently believes, and who has been told.

    Cheap and cached: it reports the last completed tick rather than measuring.
    Nothing is required to read it, because consumers are *told*, but a human
    asking "why did the pipeline stop?" should not have to read a log."""
    if watch.current is None:
        raise HTTPException(status_code=503, detail="No watcher running in this process")
    return watch.current.as_dict()


@app.get("/resources")
def resources() -> dict:
    """A fresh measurement, on purpose. Utilization covers the time since the
    previous request to this route, or the last second on the first."""
    try:
        return _probe()
    except Exception as exc:
        raise HTTPException(status_code=503, detail=f"probe failed: {exc}") from exc


@app.get("/status")
def status() -> dict:
    """Which of the services the warden watches are listening, and which models
    the router has loaded."""
    urls = {"router": settings.router_url}
    if settings.comfyui_url:
        urls["comfyui"] = settings.comfyui_url
    return {
        "services": {name: {"url": url, "up": _port_open(url)} for name, url in urls.items()},
        "router_models": [
            {"id": row.get("id"), "status": (row.get("status") or {}).get("value")}
            for row in router.models(settings.router_url)
        ],
    }
