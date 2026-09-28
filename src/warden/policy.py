"""The decision: is somebody else's work on this GPU at stake, and what should
the consumers do about it.

Pure functions over a measurement dictionary and the previous verdict. Pure so
the whole decision table is testable without a GPU, a running llama-server, or
anything to announce to.

**The rule, as the user set it:** other work takes priority, but only where a
consumer would *noticeably* degrade it. So this governs on resource contention,
NOT on whether somebody is at the keyboard — a reader typing an email is not a
reason to stop writing articles, and a game left running while they are away
still is. Idle-time detection was considered for this and dropped.

Three signals, each used only where it is valid:

* `comfyui_jobs`, ComfyUI's running plus pending jobs. A queued job is
  contention before it has drawn any power, which is the point: the router is
  told to unload before ComfyUI has loaded much, not one tick after (0002).
* `foreign_gpu_percent` is per-process utilization minus our own llama-server
  processes. Attribution makes it truthful whether or not we are generating, so
  it works as a *pause* signal and not merely as a start gate.
* Free VRAM is consulted only while our own models are unloaded, where the
  whole figure is by definition someone else's. On Windows that was forced:
  the per-process memory counter reported 22 GB for dwm on a 10 GB card. On
  Linux NVML attributes memory per process, so a rule that also works while we
  hold models is possible. The port kept the rule as it was (0002).

This module moved out of Episteme (`worker/governor.py`) in the split, unchanged
in substance. What changed is who runs it: the warden decides and announces,
where before it published measurements and Episteme decided (0001).
"""

from __future__ import annotations

from dataclasses import dataclass, replace
from datetime import UTC, datetime

from .config import Policy


@dataclass(frozen=True)
class Verdict:
    """What the warden currently believes, and since when.

    `yielded` is the state announced to consumers: true means "stop using the
    GPU". `contended_at` is when contention was last *observed*, which is what
    the resume window is measured against — a window anchored to the start of
    the pause would elapse while the game was still running, and the first
    momentary dip after that would resume into it, which is the "lull between
    two loading screens" this is written to avoid.
    """

    yielded: bool = False
    reason: str = "no measurement yet"
    since: datetime | None = None
    contended_at: datetime | None = None
    sampled_at: datetime | None = None

    @property
    def action(self) -> str:
        return "pause" if self.yielded else "resume"

    def as_dict(self) -> dict:
        return {
            "yielded": self.yielded,
            "action": self.action,
            "reason": self.reason,
            "since": self.since.isoformat() if self.since else None,
            "contended_at": self.contended_at.isoformat() if self.contended_at else None,
            "sampled_at": self.sampled_at.isoformat() if self.sampled_at else None,
        }


def is_contended(resources: dict, *, our_models_loaded: bool, policy: Policy) -> tuple[bool, str]:
    """Is someone else's work on this GPU at stake? Returns (contended, why).

    `why` is carried through to the log, `/verdict` and every announcement: a
    pipeline that stopped on its own must be able to say what it saw, or the
    feature is indistinguishable from a bug."""
    jobs = resources.get("comfyui_jobs")
    if jobs:
        return True, f"ComfyUI has {jobs} job{'s' if jobs != 1 else ''} queued"

    foreign = resources.get("foreign_gpu_percent")
    if foreign is not None and foreign >= policy.gpu_busy_percent:
        culprits = resources.get("culprits") or []
        detail = f" ({', '.join(culprits)})" if culprits else ""
        return True, f"foreign GPU load {foreign:.0f}% >= {policy.gpu_busy_percent:.0f}%{detail}"

    free = resources.get("vram_free_mb")
    if not our_models_loaded and free is not None and free < policy.min_free_vram_mb:
        return (
            True,
            f"only {free} MB VRAM free, need {policy.min_free_vram_mb} MB to load a model",
        )

    return False, "GPU is free"


def decide(
    previous: Verdict,
    resources: dict,
    *,
    now: datetime,
    our_models_loaded: bool,
    policy: Policy,
) -> Verdict:
    """One tick. Returns the new verdict; compare it with the old one to know
    whether anything has to be announced.

    Asymmetric by design: yield the moment contention appears, return only after
    the GPU has been quiet for `resume_quiet_seconds`. Restarting a 20 GB model
    load during a lull between two loading screens is worse than waiting.
    """
    contended, why = is_contended(resources, our_models_loaded=our_models_loaded, policy=policy)

    if contended:
        if previous.yielded:
            # Still busy: re-stamp the observation without moving `since`, which
            # is what the console and the announcement report as the start.
            return replace(previous, reason=why, contended_at=now, sampled_at=now)
        return Verdict(yielded=True, reason=why, since=now, contended_at=now, sampled_at=now)

    if not previous.yielded:
        return replace(previous, yielded=False, reason=why, sampled_at=now)

    quiet_for = _seconds_since(previous.contended_at, now)
    if quiet_for is not None and quiet_for < policy.resume_quiet_seconds:
        return replace(
            previous,
            reason=f"{why}, but only for {quiet_for:.0f}s of {policy.resume_quiet_seconds}s",
            sampled_at=now,
        )
    return Verdict(yielded=False, reason=why, since=now, contended_at=None, sampled_at=now)


@dataclass(frozen=True)
class ComfyIdle:
    """When ComfyUI was last seen working, and whether it has been told to drop
    its models since. `busy_at` starts at the warden's own start, so a warden
    that restarts beside an idle ComfyUI still frees it, one window later."""

    busy_at: datetime | None = None
    freed: bool = False


def comfyui_free_due(
    previous: ComfyIdle, jobs: int | None, *, now: datetime, policy: Policy
) -> tuple[ComfyIdle, bool]:
    """One tick of ComfyUI's idle clock. Returns the state to keep if a `/free`
    is sent and lands, and whether to send one now.

    Once per idle stretch: after a free, nothing more is sent until a job has
    been seen, because a free is also what empties the cache ComfyUI would
    reuse for the next job. A queue that cannot be read (`None`) moves nothing,
    the same no-opinion rule as a failed probe."""
    if jobs is None:
        return previous, False
    if jobs > 0:
        return ComfyIdle(busy_at=now, freed=False), False
    if previous.freed:
        return previous, False
    idle = _seconds_since(previous.busy_at, now)
    if idle is not None and idle < policy.comfyui_idle_seconds:
        return previous, False
    return replace(previous, freed=True), True


def _seconds_since(when: datetime | None, now: datetime) -> float | None:
    """None when there is no observation to measure from — treated as "long
    enough", since the alternative is a yield that can never lift."""
    if when is None:
        return None
    if when.tzinfo is None:
        when = when.replace(tzinfo=UTC)
    return (now - when).total_seconds()
