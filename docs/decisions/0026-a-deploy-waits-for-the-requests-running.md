# 0026. A deploy waits for the requests running, up to 40 s

- Date: 2026-10-14
- Status: accepted; running on both hosts since 2026-10-14 (Verification, below)
- Amends [0023](0023-a-batch-request-is-a-job-paused-mid-inference.md), item 4: a job is paused and written after the drain, not at the signal. Amends [0025](0025-a-safety-stop-takes-the-local-models-off.md): the zbox's `TimeoutStopSec=5` is gone, and its controller kills the models itself when it stops the daemon. Keeps 0025's safety stops cancelling at once.

## Context

Until 2026-10-14 a stop of `infermux.service` cut every request in flight. systemd's SIGTERM went to the whole cgroup (`KillMode=control-group`), so each llama-server died with the daemon's signal, and llama-swap's own shutdown, which waits up to 30 s for the HTTP server, had nothing left to wait for. On the zbox `TimeoutStopSec=5` cut what was left after 5 s. A job was paused and written to `jobs.json` at the signal (0023, 4). Both hosts are deployed by day, by a `nix run .#switch` on strix and a `nixos-rebuild switch` to the zbox, and each restarts the daemon when its package or unit changes.

## Decision

The question put to the user, 2026-10-14: on a deploy (a stop or restart of `infermux.service` on either host), the daemon waits for running requests to finish, up to about 40 s, before it exits. The user's answer, about 12:55: "yes, make deploys wait." The coordinating session added the rest of the brief: stop taking new requests, then do what happened before at the deadline; the answer to a new request is the agent's pick, preferring one that failover or a client's retry handles; `TimeoutStopSec` above the drain on both hosts; the zbox's off file and thermal path keep cancelling at once, because they are safety stops and not deploys.

The agent built it as follows.

1. **On SIGTERM the daemon refuses every new request, waits up to 40 s for the ones running, then stops as before.** At the deadline a job's attempt is paused and written (0023, 4), an idle WebSocket session gets 1012 (0018, 7), and every other request is cut. The wait ends as soon as nothing runs, so a deploy with nothing in flight stops as fast as before.
2. **A new request gets 503 with `"type":"draining"` and `Retry-After: 5`, not a refused connection.** Failover moves a 503 to the model's next place (0016), so a request from strix to a draining zbox goes on at once. A client gets an answer that says why and when to come back; a closed listener gives it a TCP error, and Go's transport does not retry a POST on a closed keep-alive connection. 5 s is about how long a restart takes. The refusal is at the outermost handler (`internal/drain`), so a draining host forwards no new work to another host or a cloud that it would then cut.
3. **The drain waits for anything but a read.** A GET, HEAD or OPTIONS is refused like any new request but not waited for: the ones that stay open are a WebSocket session or a stream that follows something else (a job's events, llama-swap's events), and those end when what they follow does.
4. **A job's attempt is waited for like a request, and no job starts a new attempt during the drain.** The attempts run inside the daemon, so the drain counts them itself. A job waiting to retry waits until the stop, since a new attempt would only be cut.
5. **No drain while the local models are off.** The stop reads the off file itself. With it there the stop is the zbox controller's fallback, and it cuts at once, as 0025 requires. The cost: a deploy at night on the zbox, while off, cuts the clouds' requests as every deploy did before.
6. **`KillMode=mixed`, so the SIGTERM reaches only the daemon.** Under `control-group` the llama-servers died with the signal and the drain would have nothing to drain. After the daemon exits, systemd sends SIGKILL to whatever it left.
7. **`TimeoutStopSec = 90` on both hosts, set in `nix/module.nix`.** The longest stop is the drain's 40 s, the jobs' 10 s, and llama-swap's own shutdown, which its backstop forces at 35 s: 85 s. The zbox's own 5 s is gone.
8. **The drain runs before llama-swap's backstop starts its clock.** The hook in `llama-swap.go` moved from after the backstop goroutine to before it; otherwise the backstop's `os.Exit(1)` at 35 s ends a 40 s drain.
9. **The zbox controller's fallback kills the models itself and does not wait.** Under `KillMode=mixed` its `systemctl stop` would reach only the daemon, and the daemon is what left a model running while off. So it sends `systemctl stop --no-block` and then SIGKILL to every process in the unit's cgroup but the daemon's. A blocking stop could now hold the controller's loop for 90 s.

## Consequences

- A deploy takes up to 40 s longer while something runs, about 85 s at worst before systemd's SIGKILL. `nix run .#switch` has no timeout of its own and waits.
- strix's switch stops its system units before it restarts the user units, so a switch that restarts both InferMux and T3 restarts T3 only after InferMux's drain. That includes the T3 V2 cutover's switch, should it restart InferMux too.
- During the drain the host's own clients get 503s: Codex, Episteme, the UI. How long Codex keeps retrying a 503 was not checked; a turn that starts during a drain fails once its retries run out. A request from another host fails over (0016).
- A WebSocket session is closed with 1012 after the drain rather than waited for.
- A job running past 40 s is paused and written as before, and resumes when the daemon is back (0023).

## Verification

Tests, run 2026-10-14:

- `internal/drain`: a running request finishes and a new one gets 503 `draining` with Retry-After; the cut ends what is left and a GET is not waited for; the wait returns at zero or at the deadline.
- `internal/jobs`: `TestADrainLetsTheRunningAttemptFinishAndStartsNoOther`.
- `infermux_drain_test.go`, through the whole daemon: a deploy lets the request running finish and refuses a new one; a job still running at the deadline is paused and written; a job waiting to retry starts no attempt during the drain; a stop while off drains nothing and cuts a cloud's request at once.
- Each of these mutations failed its test: no `Start`, the job attempts not waited for, no wait, a drain while off, no cut, the jobs not drained, the handler not wrapped. In nixcfg's controller tests: no kill, a blocking stop, the daemon killed with the models.
- Not covered by a test: the hook's position above the backstop in `llama-swap.go`, which only a live stop longer than 35 s shows, and `KillMode`.

Watched 2026-10-14, after nixcfg `550d1c6` on both hosts (details in `docs/verification.md`):

- A cloud chat streaming through a restart on the zbox finished with `[DONE]` 34.2 s into the drain, and the restart returned 0.4 s later.
- An Octen batch on the zbox's GPU answered 200 28.3 s after the signal: under `KillMode=mixed` its llama-server lived through the drain.
- A job still running at 40 s was paused and written, the stop took 41.6 s, past the backstop's 35 s, and the job finished as attempt 2 after the restart.
- On strix a cloud chat still writing at 40 s was cut at 40.02 s; another client's chat on the GPU, in flight at the signal, answered 200 inside the drain.
- A new chat and a GET during a drain each got 503 `draining` with `Retry-After: 5`, on both hosts.

Not watched: a stop while off, the controller's fallback killing the models, a WebSocket closed with 1012, a request failing over from one host's 503 `draining` to the other, and Codex's handling of that 503.
