"""ComfyUI on :8188: is it working, and telling it to drop its models.

ComfyUI keeps its models on the GPU after a job and never releases them on its
own, so an idle ComfyUI holds 6 to 12 GB until something says otherwise. The
warden watches its queue for two reasons (0002):

* A queued or running job is contention the moment it is seen. Utilization
  alone would notice it one tick later, after ComfyUI had already started
  loading weights into a card the router still occupied.
* After `comfyui_idle_seconds` with an empty queue the warden POSTs `/free`,
  which unloads the models and empties torch's cache. The router can resume
  only after that, because until then free VRAM stays below the policy's floor.
"""

from __future__ import annotations

import json
import urllib.request

from .consumers import post_json


def queue_depth(url: str, *, timeout: float = 2.0) -> int | None:
    """Running plus pending jobs, or None when ComfyUI does not answer. None is
    no opinion, not an empty queue: a ComfyUI that is down is not working, but a
    ComfyUI that is too busy to answer may be."""
    try:
        with urllib.request.urlopen(  # noqa: S310 - fixed scheme, loopback
            f"{url.rstrip('/')}/queue", timeout=timeout
        ) as response:
            body = json.loads(response.read().decode("utf-8"))
    except Exception:
        return None
    return len(body.get("queue_running") or []) + len(body.get("queue_pending") or [])


def free(url: str, *, timeout: float = 30.0) -> None:
    """Unload every model and release the cached allocations. Idempotent: a
    ComfyUI with nothing loaded answers the same."""
    post_json(
        f"{url.rstrip('/')}/free", {"unload_models": True, "free_memory": True}, timeout=timeout
    )
