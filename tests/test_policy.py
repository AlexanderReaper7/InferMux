"""The decision table (`warden/policy.py`).

Pure functions over a measurement dictionary, so the whole table is testable
without a GPU, a llama-server or anything to announce to. That is most of the
reason the decision was written this way.

These came with the code from Episteme's `tests/test_governor.py`, where they
ran against an async governor that also had to pause a pipeline and unload
models. Here the same table is eight synchronous calls.
"""

from datetime import UTC, datetime, timedelta

from warden.config import Policy
from warden.policy import Verdict, decide, is_contended

POLICY = Policy(gpu_busy_percent=25.0, min_free_vram_mb=6000, resume_quiet_seconds=300)
NOW = datetime(2026, 9, 13, 20, 0, tzinfo=UTC)

FREE = {"foreign_gpu_percent": 1.0, "vram_free_mb": 9000, "games_running": []}
BUSY = {"foreign_gpu_percent": 91.0, "vram_free_mb": 900, "games_running": ["bf6"]}


# --- what counts as contention ---------------------------------------------------


def test_our_own_generation_is_not_contention():
    """The whole reason the probe attributes per process. At 100% GPU with all of
    it ours, nothing is at stake and a governor that paused here would pause
    itself for working."""
    ours = {"gpu_percent": 100.0, "foreign_gpu_percent": 0.0, "vram_free_mb": 300}
    contended, why = is_contended(ours, our_models_loaded=True, policy=POLICY)
    assert contended is False
    assert why == "GPU is free"


def test_a_game_on_the_card_is_contention_and_says_which_one():
    """`why` reaches the log, the console and every announcement. A pipeline that
    stopped on its own must be able to say what it saw, or the feature is
    indistinguishable from a bug."""
    contended, why = is_contended(BUSY, our_models_loaded=True, policy=POLICY)
    assert contended is True
    assert "91%" in why and "bf6" in why


def test_free_vram_is_only_read_when_we_hold_none():
    """Per-process VRAM cannot be attributed on Windows - the counter reported
    22 GB for dwm on a 10 GB card - so the figure is only somebody else's while
    our own models are unloaded. While we hold them it is ignored rather than
    guessed at, which is the difference between these two calls."""
    tight = {"foreign_gpu_percent": 2.0, "vram_free_mb": 400}
    assert is_contended(tight, our_models_loaded=True, policy=POLICY)[0] is False
    assert is_contended(tight, our_models_loaded=False, policy=POLICY)[0] is True


def test_a_missing_reading_is_not_evidence_of_contention():
    """nvidia-smi with no utilization line emits null. Treating an absent number
    as a high one would pause the pipeline every time the probe half-failed."""
    assert is_contended({}, our_models_loaded=False, policy=POLICY)[0] is False


# --- the asymmetry ---------------------------------------------------------------


def test_yielding_is_immediate():
    verdict = decide(Verdict(), BUSY, now=NOW, our_models_loaded=True, policy=POLICY)
    assert verdict.yielded is True
    assert verdict.action == "pause"
    assert verdict.since == NOW and verdict.contended_at == NOW


def test_resuming_waits_out_the_quiet_window():
    """Restarting a 20 GB model load during the lull between two loading screens
    is worse than waiting, so the return is slow where the yield was instant."""
    yielded = decide(Verdict(), BUSY, now=NOW, our_models_loaded=True, policy=POLICY)
    soon = decide(
        yielded, FREE, now=NOW + timedelta(seconds=60), our_models_loaded=True, policy=POLICY
    )
    assert soon.yielded is True
    assert "60s of 300s" in soon.reason

    later = decide(
        yielded, FREE, now=NOW + timedelta(seconds=301), our_models_loaded=True, policy=POLICY
    )
    assert later.yielded is False
    assert later.action == "resume"


def test_the_window_is_measured_from_the_last_busy_sample_not_from_the_pause():
    """A window anchored to the start of the pause elapses while the game is
    still running, and the first momentary dip after that resumes into it."""
    verdict = decide(Verdict(), BUSY, now=NOW, our_models_loaded=True, policy=POLICY)
    for minute in range(1, 8):  # still playing, seven minutes later
        verdict = decide(
            verdict,
            BUSY,
            now=NOW + timedelta(minutes=minute),
            our_models_loaded=True,
            policy=POLICY,
        )
    assert verdict.since == NOW  # the pause began when it began
    assert verdict.contended_at == NOW + timedelta(minutes=7)

    dip = decide(
        verdict,
        FREE,
        now=NOW + timedelta(minutes=8),
        our_models_loaded=True,
        policy=POLICY,
    )
    assert dip.yielded is True  # one quiet minute is not the end of the game


def test_a_pause_with_no_observation_behind_it_can_still_lift():
    """`contended_at` is None on a verdict restored from nothing. Treating that
    as "no quiet time yet" would be a pause that can never be cleared."""
    stuck = Verdict(yielded=True, reason="restored", since=NOW, contended_at=None)
    assert decide(stuck, FREE, now=NOW, our_models_loaded=True, policy=POLICY).yielded is False


def test_a_running_verdict_keeps_its_start_time():
    """`since` moves on a transition and nowhere else, because it is what the
    console and the announcement report as "running since"."""
    first = decide(Verdict(), FREE, now=NOW, our_models_loaded=True, policy=POLICY)
    second = decide(
        first, FREE, now=NOW + timedelta(minutes=5), our_models_loaded=True, policy=POLICY
    )
    assert second.yielded is False
    assert second.since == first.since
    assert second.sampled_at == NOW + timedelta(minutes=5)
