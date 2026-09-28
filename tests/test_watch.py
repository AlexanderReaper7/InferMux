"""The loop (`warden/watch.py`): measure, decide, act, tell.

Driven one tick at a time. The thread is not started here; `_run` is a `while`
around `tick()` and a sleep, and what is worth testing is the tick.
"""

from datetime import UTC, datetime, timedelta

import pytest

from warden.config import Consumer, Policy, Settings
from warden.consumers import Announcer
from warden.watch import Watcher

BUSY = {"foreign_gpu_percent": 91.0, "vram_free_mb": 900, "culprits": ["bf6"]}
FREE = {"foreign_gpu_percent": 0.5, "vram_free_mb": 9000, "culprits": []}
START = datetime(2026, 9, 28, 20, 0, tzinfo=UTC)


def make_settings(**kwargs) -> Settings:
    return Settings(
        host="127.0.0.1",
        port=5003,
        policy=kwargs.get("policy", Policy(resume_quiet_seconds=300, comfyui_idle_seconds=600)),
        consumers=kwargs.get("consumers", ()),
        comfyui_url=kwargs.get("comfyui_url"),
    )


class Endpoint:
    def __init__(self) -> None:
        self.posts: list[dict] = []

    def __call__(self, url: str, payload: dict, *, timeout: float = 10.0) -> dict:
        self.posts.append(payload)
        return {}


class Clock:
    def __init__(self) -> None:
        self.at = START

    def __call__(self) -> datetime:
        return self.at

    def advance(self, seconds: float) -> None:
        self.at += timedelta(seconds=seconds)


@pytest.fixture
def watcher(monkeypatch):
    endpoint = Endpoint()
    monkeypatch.setattr("warden.consumers.post_json", endpoint)
    readings = []
    unloads = []
    made = Watcher(
        make_settings(),
        resources=lambda: readings.pop(0),
        models_loaded=lambda: True,
        unload=lambda: unloads.append(1) or ["main"],
        announcer=Announcer((Consumer(name="episteme", url="http://127.0.0.1:8200"),)),
    )
    made.readings = readings
    made.endpoint = endpoint
    made.unloads = unloads
    return made


def test_a_tick_measures_decides_and_announces(watcher):
    watcher.readings.append(BUSY)
    verdict = watcher.tick()
    assert verdict.yielded is True
    assert [p["action"] for p in watcher.endpoint.posts] == ["pause"]
    assert watcher.as_dict()["consumers"]["episteme"]["action"] == "pause"


def test_the_router_is_unloaded_on_the_transition_only(watcher):
    """The user chose "transition only": a model a client loads again during the
    pause stays loaded. Unloading every tick would fight that client (0002)."""
    watcher.readings.extend([BUSY, BUSY, BUSY])
    for _ in range(3):
        watcher.tick()
    assert watcher.unloads == [1]
    assert watcher.as_dict()["last_unload"] == ["main"]


def test_resuming_unloads_nothing(watcher):
    watcher.readings.append(FREE)
    watcher.tick()
    assert watcher.unloads == []


def test_a_failed_probe_leaves_the_verdict_alone(watcher):
    """A probe that cannot run is not evidence that the GPU is free. Reading a
    failure as quiet is the one mistake this loop exists to avoid."""

    def explode():
        raise OSError("NVML Shared Library Not Found")

    watcher.readings.append(BUSY)
    watcher.tick()
    watcher._resources = explode
    after = watcher.tick()

    assert after.yielded is True
    assert watcher.last_error.startswith("OSError")
    assert [p["action"] for p in watcher.endpoint.posts] == ["pause"]  # nothing new said
    assert watcher.as_dict()["probe_error"].startswith("OSError")


def test_the_measurement_is_kept_for_whoever_asks(watcher):
    """`/verdict` reads this dictionary instead of running its own sweep."""
    watcher.readings.append(FREE)
    watcher.tick()
    assert watcher.as_dict()["resources"] == FREE
    assert watcher.as_dict()["verdict"]["action"] == "resume"


def test_a_disabled_policy_starts_no_thread_and_tells_nobody(monkeypatch):
    """The off switch is honest: it does not announce "resume" on the way out.
    Turning the warden off is not a statement that the GPU is free, and a
    consumer paused by a human stays paused."""
    endpoint = Endpoint()
    monkeypatch.setattr("warden.consumers.post_json", endpoint)
    made = Watcher(
        make_settings(policy=Policy(enabled=False)),
        resources=lambda: FREE,
        models_loaded=lambda: True,
        unload=lambda: [],
        announcer=Announcer((Consumer(name="episteme", url="http://127.0.0.1:8200"),)),
    )
    made.start()
    assert made._thread is None
    assert endpoint.posts == []
    made.stop()


# --- ComfyUI ---------------------------------------------------------------------


class Comfy:
    def __init__(self) -> None:
        self.jobs: int | None = 0
        self.frees = 0
        self.fail = False

    def queue(self) -> int | None:
        return self.jobs

    def free(self) -> None:
        if self.fail:
            raise ConnectionRefusedError()
        self.frees += 1


@pytest.fixture
def comfy_watcher(monkeypatch):
    monkeypatch.setattr("warden.consumers.post_json", Endpoint())
    comfy = Comfy()
    clock = Clock()
    unloads = []
    made = Watcher(
        make_settings(comfyui_url="http://127.0.0.1:8188"),
        resources=lambda: FREE,
        models_loaded=lambda: True,
        unload=lambda: unloads.append(1) or ["main"],
        comfyui_queue=comfy.queue,
        comfyui_free=comfy.free,
        announcer=Announcer(()),
        now=clock,
    )
    made.comfy, made.clock, made.unloads = comfy, clock, unloads
    return made


def test_a_queued_comfyui_job_yields_and_unloads_the_router(comfy_watcher):
    comfy_watcher.comfy.jobs = 1
    verdict = comfy_watcher.tick()
    assert verdict.yielded is True
    assert verdict.reason == "ComfyUI has 1 job queued"
    assert comfy_watcher.unloads == [1]
    assert comfy_watcher.last_resources["comfyui_jobs"] == 1


def test_comfyui_is_freed_after_the_idle_window(comfy_watcher):
    """The clock starts with the warden, so a warden started beside an idle
    ComfyUI frees it one window later, not immediately."""
    comfy_watcher.tick()
    assert comfy_watcher.comfy.frees == 0
    comfy_watcher.clock.advance(600)
    comfy_watcher.tick()
    comfy_watcher.clock.advance(600)
    comfy_watcher.tick()
    assert comfy_watcher.comfy.frees == 1
    assert comfy_watcher.as_dict()["comfyui"]["freed"] is True


def test_a_free_that_does_not_land_is_tried_again(comfy_watcher):
    comfy_watcher.comfy.fail = True
    comfy_watcher.clock.advance(600)
    comfy_watcher.tick()
    assert comfy_watcher.comfyui.freed is False

    comfy_watcher.comfy.fail = False
    comfy_watcher.clock.advance(5)
    comfy_watcher.tick()
    assert comfy_watcher.comfy.frees == 1
    assert comfy_watcher.comfyui.freed is True


def test_an_unreachable_comfyui_is_neither_contention_nor_idle(comfy_watcher):
    comfy_watcher.comfy.jobs = None
    comfy_watcher.clock.advance(3600)
    verdict = comfy_watcher.tick()
    assert verdict.yielded is False
    assert comfy_watcher.comfy.frees == 0


def test_no_comfyui_url_means_no_comfyui_calls(monkeypatch):
    monkeypatch.setattr("warden.consumers.post_json", Endpoint())
    made = Watcher(
        make_settings(),
        resources=lambda: FREE,
        models_loaded=lambda: True,
        unload=lambda: [],
        announcer=Announcer(()),
    )
    assert made._comfyui_queue is None
    made.tick()
    assert "comfyui_jobs" not in made.last_resources
