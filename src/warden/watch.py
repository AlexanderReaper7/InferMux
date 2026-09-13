"""The loop: measure, decide, announce. One thread, one tick at a time.

A thread rather than an asyncio task because the measurement is a PowerShell
subprocess that takes ~3.5s wall-clock, and the HTTP service must answer the
admin panel while it runs. `agent.py` never imports this module and this module
never imports `agent.py`: the probe is handed in by `__main__`, which is what
keeps the FastAPI app testable without a GPU and the loop testable without a
server.

`current` is the live watcher, for the `/verdict` route and the console status
line. There is exactly one per process, set when the loop starts.
"""

from __future__ import annotations

import json
import logging
import threading
import time
import urllib.request
from collections.abc import Callable
from datetime import UTC, datetime

from .config import Policy, Settings
from .consumers import Announcer
from .policy import Verdict, decide

log = logging.getLogger("warden.watch")

current: Watcher | None = None


def router_holds_vram(router_url: str, *, timeout: float = 2.0) -> bool:
    """Is our own decode server holding a model in VRAM right now?

    Only llama-server in *router* mode reports a per-model `status.value`; a
    plain single-model server reports none, which counts as holding nothing.
    Everything else counts as loaded, and that deliberately includes the
    transient `loading`: a model halfway into VRAM occupies it just as much as a
    resident one, and reading it as free is what once let a governor pause and
    unload the model it was in the middle of loading.

    A down or silent router holds nothing, which is the same no-opinion
    direction the rest of this takes.
    """
    try:
        with urllib.request.urlopen(  # noqa: S310 - fixed scheme, loopback
            f"{router_url.rstrip('/')}/v1/models", timeout=timeout
        ) as response:
            rows = json.loads(response.read().decode("utf-8")).get("data") or []
    except Exception:
        return False
    return any(((row.get("status") or {}).get("value")) not in (None, "unloaded") for row in rows)


class Watcher:
    """Owns the verdict. Everything else reads it."""

    def __init__(
        self,
        settings: Settings,
        *,
        resources: Callable[[], dict],
        models_loaded: Callable[[], bool] | None = None,
        announcer: Announcer | None = None,
    ) -> None:
        self.settings = settings
        self.policy: Policy = settings.policy
        self._resources = resources
        self._models_loaded = models_loaded or (
            lambda: router_holds_vram(settings.router_url)
        )
        self.announcer = announcer or Announcer(settings.consumers)
        self.verdict = Verdict()
        self.last_resources: dict = {}
        self.last_error: str | None = None
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None

    # --- one tick ---------------------------------------------------------

    def tick(self) -> Verdict:
        """Measure once, decide, tell whoever needs telling.

        A failed measurement leaves the verdict alone rather than reading as
        "quiet". A probe that cannot run is not evidence that the GPU is free,
        and resuming on it would be the one mistake this loop exists to avoid.
        """
        try:
            resources = self._resources()
            self.last_error = None
        except Exception as exc:
            self.last_error = f"{type(exc).__name__}: {exc}"
            log.warning("Probe failed, verdict unchanged: %s", self.last_error)
            return self.verdict

        self.last_resources = resources
        before = self.verdict
        self.verdict = decide(
            before,
            resources,
            now=datetime.now(UTC),
            our_models_loaded=self._models_loaded(),
            policy=self.policy,
        )
        if before.yielded != self.verdict.yielded:
            log.info(
                "Verdict: %s — %s", self.verdict.action.upper(), self.verdict.reason
            )
        self.announcer.sync(self.verdict)
        return self.verdict

    # --- the thread -------------------------------------------------------

    def start(self) -> None:
        global current
        current = self
        if not self.policy.enabled:
            log.info("Policy disabled: measuring on request only, announcing nothing")
            return
        self._thread = threading.Thread(target=self._run, name="warden-watch", daemon=True)
        self._thread.start()
        log.info(
            "Watching every %.0fs: busy >= %.0f%%, resume after %ds quiet, consumers: %s",
            self.policy.poll_seconds,
            self.policy.gpu_busy_percent,
            self.policy.resume_quiet_seconds,
            ", ".join(c.name for c in self.announcer.consumers) or "none",
        )

    def stop(self) -> None:
        self._stop.set()

    def _run(self) -> None:
        while not self._stop.is_set():
            started = time.monotonic()
            try:
                self.tick()
            except Exception:  # a tick must never be the last one
                log.exception("Tick failed")
            # Measured from the start of the tick, so the probe's own ~3.5s is
            # inside the interval rather than added to it.
            self._stop.wait(max(1.0, self.policy.poll_seconds - (time.monotonic() - started)))

    # --- what the HTTP surface and the console show -----------------------

    def as_dict(self) -> dict:
        return {
            "enabled": self.policy.enabled,
            "verdict": self.verdict.as_dict(),
            "policy": {
                "gpu_busy_percent": self.policy.gpu_busy_percent,
                "min_free_vram_mb": self.policy.min_free_vram_mb,
                "resume_quiet_seconds": self.policy.resume_quiet_seconds,
                "poll_seconds": self.policy.poll_seconds,
            },
            "consumers": self.announcer.state(),
            "probe_error": self.last_error,
            "resources": self.last_resources,
        }
