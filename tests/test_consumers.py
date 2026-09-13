"""Telling the consumers (`warden/consumers.py`).

The user chose push over a lease: the warden says "pause" when it decides to
pause and "resume" when it resumes, and nothing expires on its own (0001). That
choice puts the whole burden of reliability on this file, so what is tested here
is exactly the three properties the decision record claims.
"""

from datetime import UTC, datetime

import pytest

from warden.config import Consumer
from warden.consumers import Announcer
from warden.policy import Verdict

NOW = datetime(2026, 9, 13, 20, 0, tzinfo=UTC)
EPISTEME = Consumer(name="episteme", url="http://127.0.0.1:8200")
PAUSE = Verdict(yielded=True, reason="foreign GPU load 91% >= 25% (bf6)", since=NOW)
RESUME = Verdict(yielded=False, reason="GPU is free", since=NOW)


class Endpoint:
    """Stands in for a consumer's announce route. `fail` makes it unreachable,
    which is a normal state: the warden starts at logon and Episteme's containers
    may not be up for minutes."""

    def __init__(self) -> None:
        self.posts: list[dict] = []
        self.fail = False

    def __call__(self, url: str, payload: dict, *, timeout: float = 10.0) -> dict:
        if self.fail:
            raise ConnectionRefusedError(url)
        self.posts.append(payload)
        return {"ok": True}


@pytest.fixture
def announcer(monkeypatch):
    endpoint = Endpoint()
    monkeypatch.setattr("warden.consumers.post_json", endpoint)
    made = Announcer((EPISTEME,), reannounce_seconds=300)
    made.endpoint = endpoint  # the test reads what the consumer received
    return made


def test_a_transition_is_announced_once(announcer):
    announcer.sync(PAUSE)
    announcer.sync(PAUSE)
    announcer.sync(PAUSE)
    assert [p["action"] for p in announcer.endpoint.posts] == ["pause"]
    assert announcer.endpoint.posts[0]["reason"].startswith("foreign GPU load 91%")
    assert announcer.endpoint.posts[0]["since"] == NOW.isoformat()


def test_both_directions_are_pushed(announcer):
    announcer.sync(PAUSE)
    announcer.sync(RESUME)
    assert [p["action"] for p in announcer.endpoint.posts] == ["pause", "resume"]


def test_the_tick_is_the_retry(announcer):
    """A consumer that was down keeps its old recorded state, so the next tick
    says it again. No backoff table and no retry loop: the watch thread is
    already a timer, and one that runs whether or not anything changed."""
    announcer.endpoint.fail = True
    announcer.sync(PAUSE)
    assert announcer.endpoint.posts == []
    assert announcer.state()["episteme"]["error"].startswith("ConnectionRefusedError")

    announcer.endpoint.fail = False
    announcer.sync(PAUSE)
    assert [p["action"] for p in announcer.endpoint.posts] == ["pause"]
    assert announcer.state()["episteme"]["error"] is None


def test_the_state_is_repeated_even_when_nothing_changed(monkeypatch):
    """Against the failure the no-lease design actually has: a human resumes
    Episteme by hand during a game, and the warden has nothing new to say. The
    repetition is what re-imposes the pause, within `reannounce_seconds` rather
    than when the game exits."""
    endpoint = Endpoint()
    monkeypatch.setattr("warden.consumers.post_json", endpoint)
    clock = [1000.0]
    monkeypatch.setattr("warden.consumers.time.monotonic", lambda: clock[0])

    announcer = Announcer((EPISTEME,), reannounce_seconds=300)
    announcer.sync(PAUSE)
    clock[0] += 299
    announcer.sync(PAUSE)
    assert len(endpoint.posts) == 1
    clock[0] += 2
    announcer.sync(PAUSE)
    assert [p["action"] for p in endpoint.posts] == ["pause", "pause"]


def test_a_restarted_warden_re_announces_what_it_believes(monkeypatch):
    """The other half of the same mitigation. Delivery is tracked in memory, so a
    warden that just started has recorded nothing and therefore tells every
    consumer its verdict on the first tick - which is how a consumer that was
    paused, restarted, and came back running is put right again."""
    endpoint = Endpoint()
    monkeypatch.setattr("warden.consumers.post_json", endpoint)
    Announcer((EPISTEME,)).sync(PAUSE)
    Announcer((EPISTEME,)).sync(PAUSE)  # a second process, fresh memory
    assert [p["action"] for p in endpoint.posts] == ["pause", "pause"]


def test_a_warden_with_no_consumers_announces_nothing(monkeypatch):
    endpoint = Endpoint()
    monkeypatch.setattr("warden.consumers.post_json", endpoint)
    assert Announcer(()).sync(PAUSE) == []
    assert endpoint.posts == []
