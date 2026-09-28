"""The loop: measure, decide, act, announce. One thread, one tick at a time.

A thread rather than an asyncio task so the HTTP service answers while a tick
waits on the router or ComfyUI. `agent.py` never imports this module and this
module never imports `agent.py`: the probe is handed in by `__main__`, which is
what keeps the FastAPI app testable without a GPU and the loop testable without
a server.

A tick does two things of its own beyond announcing (0002):

* On the transition to *yielded* it tells the router to unload its models.
  Only on the transition: a model that a client loads again during the pause
  stays loaded, as the user chose.
* It keeps ComfyUI's idle clock and sends `/free` once per idle stretch.

`current` is the live watcher, for the `/verdict` route. There is exactly one
per process, set when the loop starts.
"""

from __future__ import annotations

import logging
import threading
import time
from collections.abc import Callable
from datetime import UTC, datetime

from . import comfyui, router
from .config import Policy, Settings
from .consumers import Announcer
from .policy import ComfyIdle, Verdict, comfyui_free_due, decide

log = logging.getLogger("warden.watch")

current: Watcher | None = None


class Watcher:
    """Owns the verdict. Everything else reads it."""

    def __init__(
        self,
        settings: Settings,
        *,
        resources: Callable[[], dict],
        models_loaded: Callable[[], bool] | None = None,
        unload: Callable[[], list[str]] | None = None,
        comfyui_queue: Callable[[], int | None] | None = None,
        comfyui_free: Callable[[], None] | None = None,
        announcer: Announcer | None = None,
        now: Callable[[], datetime] = lambda: datetime.now(UTC),
    ) -> None:
        self.settings = settings
        self.policy: Policy = settings.policy
        self._resources = resources
        self._models_loaded = models_loaded or (lambda: router.holds_vram(settings.router_url))
        self._unload = unload or (lambda: router.unload_all(settings.router_url))
        url = settings.comfyui_url
        self._comfyui_queue = comfyui_queue or ((lambda: comfyui.queue_depth(url)) if url else None)
        self._comfyui_free = comfyui_free or ((lambda: comfyui.free(url)) if url else None)
        self.announcer = announcer or Announcer(settings.consumers)
        self._now = now
        self.verdict = Verdict()
        self.comfyui = ComfyIdle(busy_at=now())
        self.last_resources: dict = {}
        self.last_error: str | None = None
        self.last_unload: list[str] | None = None
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None

    # --- one tick ---------------------------------------------------------

    def tick(self) -> Verdict:
        """Measure once, decide, act on the router and ComfyUI, tell whoever
        needs telling.

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

        now = self._now()
        if self._comfyui_queue is not None:
            jobs = self._comfyui_queue()
            resources = {**resources, "comfyui_jobs": jobs}
            self._comfyui_tick(jobs, now)

        self.last_resources = resources
        before = self.verdict
        self.verdict = decide(
            before,
            resources,
            now=now,
            our_models_loaded=self._models_loaded(),
            policy=self.policy,
        )
        if before.yielded != self.verdict.yielded:
            log.info("Verdict: %s - %s", self.verdict.action.upper(), self.verdict.reason)
        if self.verdict.yielded and not before.yielded:
            self.last_unload = self._unload()
            log.info("Router unloaded: %s", ", ".join(self.last_unload) or "nothing was loaded")
        self.announcer.sync(self.verdict)
        return self.verdict

    def _comfyui_tick(self, jobs: int | None, now: datetime) -> None:
        after, due = comfyui_free_due(self.comfyui, jobs, now=now, policy=self.policy)
        if not due:
            self.comfyui = after
            return
        try:
            self._comfyui_free()
        except Exception as exc:
            # Not marked freed, so the next tick tries again.
            log.warning("ComfyUI did not take /free: %s: %s", type(exc).__name__, exc)
            return
        self.comfyui = after
        log.info("ComfyUI idle for %ds: models freed", self.policy.comfyui_idle_seconds)

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
            "Watching every %.0fs: busy >= %.0f%%, resume after %ds quiet, consumers: %s, "
            "ComfyUI: %s",
            self.policy.poll_seconds,
            self.policy.gpu_busy_percent,
            self.policy.resume_quiet_seconds,
            ", ".join(c.name for c in self.announcer.consumers) or "none",
            self.settings.comfyui_url or "none",
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
            self._stop.wait(max(1.0, self.policy.poll_seconds - (time.monotonic() - started)))

    # --- what /verdict shows ----------------------------------------------

    def as_dict(self) -> dict:
        return {
            "enabled": self.policy.enabled,
            "verdict": self.verdict.as_dict(),
            "policy": {
                "gpu_busy_percent": self.policy.gpu_busy_percent,
                "min_free_vram_mb": self.policy.min_free_vram_mb,
                "resume_quiet_seconds": self.policy.resume_quiet_seconds,
                "poll_seconds": self.policy.poll_seconds,
                "comfyui_idle_seconds": self.policy.comfyui_idle_seconds,
            },
            "consumers": self.announcer.state(),
            "comfyui": {
                "url": self.settings.comfyui_url,
                "busy_at": self.comfyui.busy_at.isoformat() if self.comfyui.busy_at else None,
                "freed": self.comfyui.freed,
            },
            "last_unload": self.last_unload,
            "probe_error": self.last_error,
            "resources": self.last_resources,
        }
