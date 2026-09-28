"""The llama.cpp router on :5001: what it holds, and telling it to let go.

systemd runs the router now, so yielding is no longer the warden stopping a
process it launched. It is `POST /models/unload` for each model the router has
in memory (0002). The router stays up, and the next request for a model loads
it again. A client that is not a consumer, such as a chat through LiteLLM, can
therefore take VRAM back in the middle of a game. The user chose that over
stopping the unit, where every client would see connection refused.
"""

from __future__ import annotations

import json
import logging
import urllib.request

from .consumers import post_json

log = logging.getLogger("warden.router")


def models(router_url: str, *, timeout: float = 2.0) -> list[dict]:
    """The router's model list, `[]` when it is down or silent."""
    try:
        with urllib.request.urlopen(  # noqa: S310 - fixed scheme, loopback
            f"{router_url.rstrip('/')}/v1/models", timeout=timeout
        ) as response:
            return json.loads(response.read().decode("utf-8")).get("data") or []
    except Exception:
        return []


def _state(row: dict) -> str | None:
    return (row.get("status") or {}).get("value")


def holds_vram(router_url: str, *, timeout: float = 2.0) -> bool:
    """Is the router holding a model in VRAM right now?

    Only llama-server in *router* mode reports a per-model `status.value`; a
    plain single-model server reports none, which counts as holding nothing.
    Everything else counts as loaded, and that deliberately includes the
    transient `loading`: a model halfway into VRAM occupies it just as much as a
    resident one, and reading it as free is what once let a governor pause and
    unload the model it was in the middle of loading.

    A down or silent router holds nothing, which is the same no-opinion
    direction the rest of this takes.
    """
    return any(_state(row) not in (None, "unloaded") for row in models(router_url, timeout=timeout))


def unload_all(router_url: str, *, timeout: float = 10.0) -> list[str]:
    """Ask the router to unload every model it has in memory. Returns the names
    it accepted. A model that finished unloading between the listing and the
    POST answers "model is not running", which is the outcome wanted anyway."""
    unloaded = []
    for row in models(router_url):
        if _state(row) in (None, "unloaded"):
            continue
        name = row.get("id")
        try:
            post_json(f"{router_url.rstrip('/')}/models/unload", {"model": name}, timeout=timeout)
        except Exception as exc:
            log.warning("Router did not unload %s: %s: %s", name, type(exc).__name__, exc)
            continue
        unloaded.append(name)
    return unloaded
