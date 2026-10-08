# 0001. The warden decides, and says so

- Date: 2026-09-13
- Status: accepted
- Rule: the machine that measures the GPU is the machine that decides. A verdict is **pushed** to every consumer on transition, and repeated until it lands.
- Supersedes Episteme's 0023 ("the host control agent is a sensor and an actuator, never a decision-maker") and takes the policy half of its 0024. Both records stay where they are, in <https://github.com/AlexanderReaper7/episteme> under `docs/decisions/`.

## Context

This code was Episteme's `hostagent/`. Episteme is a newsfeed that happens to use a local LLM; deciding who gets the GPU is not its subject, and a second consumer of the same card would have had to reimplement the whole decision or ask Episteme's permission to run.

0023 put the policy in Episteme deliberately, and its argument was good: **an agent that dies leaves no opinion behind.** A sensor that stops answering causes a consumer to fall back to "no information", not to a stale flag nobody can clear. That property is real and the split gives it up. What it cost was worse, and the costs are what actually happened:

- **The yield took up to two minutes.** The decision ran on Episteme's `*/2` cron, so a game starting at 12:00:05 waited until 12:02 to be noticed, and a 20 GB model sat in VRAM through a loading screen.
- **720 no-op job rows a day** drowned a 20-row admin queue view, and the fix was to only register the cron when the governor was enabled, which is a workaround for a policy living in a job scheduler.
- **Two processes paid the same 3.5 s probe**, because the console rendered a measurement Episteme had already taken.
- **Every threshold was in Episteme's settings and every measurement was on the host**, so answering "why did the pipeline stop" meant correlating two projects.

## Decision

`llama-warden` is its own project at `C:\selfhosting\llama-warden`. It measures (`agent.resources`), decides (`policy.decide`), and announces (`consumers.Announcer`), on a 30 s thread of its own.

A consumer is a name, a URL and a path. The warden POSTs `{"action": "pause"|"resume", "reason": ..., "since": ..., "warden": "llama-warden"}` and nothing else is assumed about it. **What a consumer does about contention is the consumer's business**: Episteme pauses its pipeline and hands back VRAM, and a second consumer might do something else entirely. That is the seam the split exists to create.

The decision table itself moved unchanged, because it was right. Contention, not presence: a reader typing an email is not a reason to stop writing articles, and a game left running while they are away still is. Yield at once, resume after five minutes of quiet, measured from the last *busy* sample rather than from the start of the pause.

### Push, not a lease

The obvious alternative was a lease: the warden grants "you may use the GPU until T+90s" and a consumer that stops hearing from it pauses itself. That is the failure-safe direction, and it was recommended.

The user chose push. The reason it is a defensible trade: a lease makes the warden a hard dependency of every consumer forever. Episteme with no warden installed would have to either pause itself permanently or ignore the lease, and the first is wrong while the second makes the lease decorative. Push keeps the warden **optional**, which is what it was as a sensor.

**The residual risk, stated rather than hidden: a warden that dies while a consumer is paused leaves it paused.** Nothing expires. The consumer waits for a "resume" that is not coming, and a person has to clear it by hand from the admin panel. Three things narrow the window and none of them closes it:

- The scheduled task restarts the process up to three times, a minute apart.
- `Announcer` tracks delivery **in memory**, so a warden that has just started has recorded nothing and re-announces its verdict to every consumer on the first tick. A consumer left paused by a crash is put right within one tick of the warden coming back.
- The verdict is re-announced every 300 s even when nothing changed, which also re-imposes a pause that a human lifted by hand in the middle of a game.

If that risk ever bites in practice, the fix is the lease, not a longer re-announce interval.

### Delivery

The tick is the retry. A consumer that was down, or answered anything but 2xx, keeps its old recorded state and is told again next tick. No backoff table, no queue: the watch thread is already a timer that runs whether or not anything changed. A consumer being unreachable is a **normal** state, not an error, because the warden starts at logon and Episteme's containers may not be up for minutes; it is logged once per transition rather than once per attempt.

The announcement must be **idempotent at the other end**. Re-announcing `pause` to an already-paused consumer must not re-stamp when the pause began, or the repetition above erases the one fact the panel shows.

## What the split also moved

- **The launcher and `models-preset.ini`** live in `llama/` here, under version control, and reach the llama.cpp binaries through `LLAMA_CPP_DIR`. One source of truth, no junction. The binaries are an unpacked upstream release and stay where they are; they are not ours and they are not in a repository.
- **The mark.** The obelisk-in-a-network was Episteme's mark with one word changed, and `graphics/build_svg.py` here is a copy of the generator. Nothing keeps the two in step now, deliberately. See `graphics/README.md`. (2026-10-08: the obelisk went back to Episteme's `graphics/obelisk-net/`, generator included, and InferMux has a mark of its own.)
- **Every threshold.** `warden.toml`, read once at import through stdlib `tomllib`. TOML rather than environment variables because the consumer list is a list of tables and a list of tables does not survive `KEY=value`.

## One probe per tick

`AgentAPI` lost its `resources` callable and gained `verdict`. The text UI renders the watcher's cached measurement and the verdict taken from it; it measures nothing. The seam is the enforcement: with no `resources` on the dataclass there is nothing for a later edit to call, and `tests/test_console.py` asserts that absence rather than trusting it.

`/resources` still exists on the HTTP service, for an operator who wants a fresh sweep on purpose.

## What was kept from 0023 and 0024

The measurement rules, because they came from what the hardware actually supports and none of that changed:

- **Per-process GPU utilization** from `\GPU Engine(*)\Utilization Percentage`, so `foreign_gpu_percent` is truthful **even while we generate**. That is what makes it a pause signal and not merely a start gate.
- **VRAM cannot be attributed.** `nvidia-smi --query-compute-apps` returns `[N/A]` per process under WDDM and the `GPU Process Memory` counter over-reports badly: dwm claimed 22 GB on a 10 GB card, because it counts committed rather than resident. Free VRAM is therefore consulted **only while our own models are unloaded**, where the whole figure is by definition somebody else's.
- **Games** come from intersecting Windows' Game Bar registry with running processes. Context for the panel, never decided on.
- **`loading` counts as loaded.** A model halfway into VRAM occupies it just as much as a resident one, and reading it as free is what once let a governor pause and unload the model it was in the middle of loading.
- **A failed probe leaves the verdict alone.** A probe that cannot run is not evidence that the GPU is free.

## Cost

One tick is one PowerShell subprocess, ~3.5 s of it the PDH sampling floor, every 30 s. That is ~12% of one core's wall-clock on a timer, traded for noticing a game within 30 s instead of within two minutes. `poll_seconds` is the dial.
