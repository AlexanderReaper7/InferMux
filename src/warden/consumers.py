"""Telling the consumers. One POST per transition, repeated until it lands.

The warden decides and then *says so*: `{"action": "pause"|"resume", "reason":
...}` to every configured consumer. Push rather than a verdict somebody polls,
because the point of the split is that a game gets the card within one tick
instead of within the consumer's next cron (0001).

Three things make a push survivable without a lock or a lease:

* **Delivery is tracked, not fired and forgotten.** A consumer that was down,
  or that answered anything other than 2xx, keeps its old recorded state, so the
  next tick tries again. No retry loop, no backoff table: the tick *is* the
  retry.
* **The announcement is repeated** every `reannounce_seconds` even when nothing
  changed. A human who resumes Episteme by hand in the middle of a game would
  otherwise keep the GPU until the game exited and restarted, since the warden
  had nothing new to say.
* **It is idempotent at the other end.** Re-announcing `pause` to an
  already-paused consumer must not re-stamp when the pause began, or the
  repetition above would erase the one fact the panel shows.

A consumer being unreachable is a normal state, not an error: the warden runs at
logon and Episteme's containers may not be up for minutes afterwards. It is
logged at info once per transition, not per attempt.
"""

from __future__ import annotations

import json
import logging
import time
import urllib.error
import urllib.request

from .config import Consumer
from .policy import Verdict

log = logging.getLogger("warden.consumers")

# How long a delivered state stays believed before it is said again.
REANNOUNCE_SECONDS = 300.0


class Announcer:
    """Keeps what each consumer was last known to have accepted."""

    def __init__(
        self, consumers: tuple[Consumer, ...], *, reannounce_seconds: float = REANNOUNCE_SECONDS
    ) -> None:
        self.consumers = consumers
        self.reannounce_seconds = reannounce_seconds
        # name -> (action, monotonic time it was accepted)
        self._delivered: dict[str, tuple[str, float]] = {}
        self._last_error: dict[str, str] = {}

    def state(self) -> dict[str, dict]:
        """What the console and `/verdict` report: who has heard what."""
        now = time.monotonic()
        rows = {}
        for consumer in self.consumers:
            delivered = self._delivered.get(consumer.name)
            rows[consumer.name] = {
                "url": consumer.endpoint,
                "action": delivered[0] if delivered else None,
                "age_seconds": round(now - delivered[1], 1) if delivered else None,
                "error": self._last_error.get(consumer.name),
            }
        return rows

    def sync(self, verdict: Verdict) -> list[dict]:
        """Bring every consumer up to date with `verdict`. Returns one row per
        consumer actually contacted, which is what the caller logs."""
        results = []
        for consumer in self.consumers:
            if not self._due(consumer, verdict):
                continue
            results.append(self._announce(consumer, verdict))
        return results

    def _due(self, consumer: Consumer, verdict: Verdict) -> bool:
        delivered = self._delivered.get(consumer.name)
        if delivered is None or delivered[0] != verdict.action:
            return True
        return (time.monotonic() - delivered[1]) >= self.reannounce_seconds

    def _announce(self, consumer: Consumer, verdict: Verdict) -> dict:
        payload = {
            "action": verdict.action,
            "reason": verdict.reason,
            "since": verdict.since.isoformat() if verdict.since else None,
            "warden": "llama-warden",
        }
        previous = self._delivered.get(consumer.name)
        changed = previous is None or previous[0] != verdict.action
        try:
            body = post_json(consumer.endpoint, payload, timeout=consumer.timeout_seconds)
        except Exception as exc:  # transport, status, or an unparseable body
            detail = f"{type(exc).__name__}: {exc}"
            # Logged once per transition rather than every tick: a consumer that
            # is simply not up yet would otherwise write a line every 30s all day.
            if self._last_error.get(consumer.name) != detail:
                log.info("Consumer %s did not take %s: %s", consumer.name, verdict.action, detail)
            self._last_error[consumer.name] = detail
            return {"consumer": consumer.name, "delivered": False, "error": detail}

        self._delivered[consumer.name] = (verdict.action, time.monotonic())
        self._last_error.pop(consumer.name, None)
        if changed:
            log.info(
                "Consumer %s took %s (%s): %s", consumer.name, verdict.action, verdict.reason, body
            )
        return {"consumer": consumer.name, "delivered": True, "response": body}


def post_json(url: str, payload: dict, *, timeout: float = 10.0) -> dict:
    """A POST and its JSON answer, on the standard library.

    `urllib` rather than httpx or requests: this is one loopback call every few
    minutes, and the warden's dependency list is something a human has to
    install on a host at logon, not into a container."""
    request = urllib.request.Request(  # noqa: S310 - http(s) only, from config
        url,
        data=json.dumps(payload).encode("utf-8"),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(request, timeout=timeout) as response:  # noqa: S310 - fixed scheme
        raw = response.read().decode("utf-8", errors="replace")
    try:
        return json.loads(raw) if raw else {}
    except json.JSONDecodeError:
        return {"body": raw[:500]}
