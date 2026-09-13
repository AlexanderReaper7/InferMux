"""The loop (`warden/watch.py`): measure, decide, tell.

Driven one tick at a time. The thread is not started here; `_run` is a `while`
around `tick()` and a sleep, and what is worth testing is the tick.
"""

import json
from io import BytesIO
from pathlib import Path

import pytest

from warden.config import Consumer, Policy, Settings
from warden.consumers import Announcer
from warden.watch import Watcher, router_holds_vram

BUSY = {"foreign_gpu_percent": 91.0, "vram_free_mb": 900, "games_running": ["bf6"]}
FREE = {"foreign_gpu_percent": 0.5, "vram_free_mb": 9000, "games_running": []}


def make_settings(**kwargs) -> Settings:
    return Settings(
        llama_dir=Path("."),
        models_dir=Path("."),
        host="127.0.0.1",
        port=5003,
        policy=kwargs.get("policy", Policy(resume_quiet_seconds=300)),
        consumers=kwargs.get("consumers", ()),
    )


class Endpoint:
    def __init__(self) -> None:
        self.posts: list[dict] = []

    def __call__(self, url: str, payload: dict, *, timeout: float = 10.0) -> dict:
        self.posts.append(payload)
        return {}


@pytest.fixture
def watcher(monkeypatch):
    endpoint = Endpoint()
    monkeypatch.setattr("warden.consumers.post_json", endpoint)
    readings = []
    made = Watcher(
        make_settings(),
        resources=lambda: readings.pop(0),
        models_loaded=lambda: True,
        announcer=Announcer((Consumer(name="episteme", url="http://127.0.0.1:8200"),)),
    )
    made.readings = readings
    made.endpoint = endpoint
    return made


def test_a_tick_measures_decides_and_announces(watcher):
    watcher.readings.append(BUSY)
    verdict = watcher.tick()
    assert verdict.yielded is True
    assert [p["action"] for p in watcher.endpoint.posts] == ["pause"]
    assert watcher.as_dict()["consumers"]["episteme"]["action"] == "pause"


def test_a_failed_probe_leaves_the_verdict_alone(watcher):
    """A probe that cannot run is not evidence that the GPU is free. Reading a
    failure as quiet is the one mistake this loop exists to avoid, and it is the
    likely one: the PowerShell counter sweep is what breaks, not the decision."""

    def explode():
        raise OSError("PDH counter unavailable")

    watcher.readings.append(BUSY)
    watcher.tick()
    watcher._resources = explode
    after = watcher.tick()

    assert after.yielded is True
    assert watcher.last_error.startswith("OSError")
    assert [p["action"] for p in watcher.endpoint.posts] == ["pause"]  # nothing new said
    assert watcher.as_dict()["probe_error"].startswith("OSError")


def test_the_measurement_is_kept_for_whoever_asks(watcher):
    """`/verdict` and the console both read this dictionary instead of running
    their own sweep, which is what keeps the ~3.5s probe to one per tick."""
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
        announcer=Announcer((Consumer(name="episteme", url="http://127.0.0.1:8200"),)),
    )
    made.start()
    assert made._thread is None
    assert endpoint.posts == []
    made.stop()


# --- do we hold VRAM ourselves ---------------------------------------------------


def _answer(monkeypatch, payload):
    class Response(BytesIO):
        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    monkeypatch.setattr(
        "warden.watch.urllib.request.urlopen",
        lambda *a, **kw: Response(json.dumps(payload).encode()),
    )


def test_a_loading_model_counts_as_loaded(monkeypatch):
    """A model halfway into VRAM occupies it exactly as much as a resident one.
    Reading `loading` as free is what once let a governor pause and unload the
    model it was in the middle of loading."""
    _answer(monkeypatch, {"data": [{"id": "main", "status": {"value": "loading"}}]})
    assert router_holds_vram("http://127.0.0.1:5001") is True


def test_an_unloaded_router_holds_nothing(monkeypatch):
    _answer(monkeypatch, {"data": [{"id": "main", "status": {"value": "unloaded"}}]})
    assert router_holds_vram("http://127.0.0.1:5001") is False


def test_a_silent_router_holds_nothing(monkeypatch):
    """Same no-opinion direction as the rest of this: a router that is down is
    not holding a model, and guessing otherwise would suppress the VRAM half of
    the policy precisely when it applies."""

    def refuse(*a, **kw):
        raise ConnectionRefusedError()

    monkeypatch.setattr("warden.watch.urllib.request.urlopen", refuse)
    assert router_holds_vram("http://127.0.0.1:5001") is False
