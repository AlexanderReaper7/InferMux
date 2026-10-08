# 0020. A backend that sends READY=1 is ready when it says so; every other backend is polled as before

- Date: 2026-10-07
- Status: accepted; built and tested 2026-10-07 and 2026-10-08 against a fake backend and the tests' helper process. dikt-server has not been started through it, and it is not deployed
- Rule: A model with `metadata: {readiness: notify}` gets `NOTIFY_SOCKET`, the path of a unix datagram socket bound for that one start, in a 0700 directory of its own. The model is ready at the moment `READY=1` arrives from the process InferMux started or one of its descendants, by the sender's `SCM_CREDENTIALS`, and `checkEndpoint` is then asked once, unless it is `none`. An exit before `READY=1`, or no `READY=1` within `healthCheckTimeout`, fails the start, with the last `STATUS=` and `ERRNO=` in the error. A message from any other process is ignored and logged. Every model without `metadata.readiness` is polled as before.
- Keeps 0004 (upstream's files are edited only at marked hook points), 0012 (a model's own InferMux settings live in `metadata`) and 0014 (the stats time requests at the front door; a request that waited for a load now says how long, and how that was learnt).

## Context

llama-swap's `doStart` waits 250 ms after starting a backend, asks `checkEndpoint`, and asks again every second until it answers 200. A backend that is ready at time t after its start is seen at the first poll after t, at 250 ms + k s, so a backend that loads in over 250 ms is seen 0 to 1 s late, 0.5 s on average, and one that loads in under 250 ms is seen at 250 ms. Measured through InferMux at `7972d54`, a fake backend whose load takes 500 ms answered its first request 1257 ms (p50) after the request was sent, and the request's first byte reached it 753 ms after it was ready (Measured).

This lands on Dikt's dictation key. The key press starts dikt-server when it is not loaded, and the user's first words wait for the load plus the poll's delay. The user rejected faster polling on 2026-10-07 as a fix of the symptom: it shortens the delay and keeps it, at the cost of a request every interval during every load.

## Decision

The user, 2026-10-07:

1. **InferMux speaks systemd's sd_notify protocol to the processes it starts.** Each start gets its own unix datagram socket, and its path in `NOTIFY_SOCKET`. `READY=1` means the model is loaded: the requests waiting for it are forwarded at once.
2. **A backend that does not speak it is polled as before.** llama-server and ComfyUI keep the health polling unchanged.
3. **dikt-server sends `READY=1`.** Its side of the contract is below.

How it is built, the agent's choices:

4. **Opt in per model, as `metadata: {readiness: notify}`.** What `READY=1` means is the backend's to say: a service that sends it once its listener is up, before its model is loaded, would have requests forwarded to a server still loading. The opt-in is the user saying that this backend's `READY=1` means its model is loaded. It lives in `metadata` because `config.ModelConfig` is upstream's struct and schema, and a field there is an upstream edit paid at every merge (0004). 0012's `metadata.hf` is the precedent. The cost: llama-swap passes `metadata` through to `/v1/models` as `meta.llamaswap`, so `readiness: notify` shows there. Any other value fails the start (`metadata.readiness is notfy; the only value is "notify"`), so a misspelt opt-in does not fall back to polling without a word. Rejected: `NOTIFY_SOCKET` for every model, with `READY=1` and polling raced, which needs no opt-in and takes a premature `READY=1` from a backend that never promised what it means.
5. **The socket is a path, bound before the process starts, and removed when the start ends.** `os.MkdirTemp` makes `$TMPDIR/infermux-notify-<random>/` with mode 0700, and the socket is `notify` in it. It is bound before `cmd.Start`, so a backend that is ready at once loses nothing, and it is closed and removed when `doStart` returns, ready or not. On strix the unit has `PrivateTmp`, so `/tmp` is the service's own, shared with its children and removed with the service. Rejected: the abstract namespace. An abstract socket has no permissions, so any process of any user in the same network namespace can send to it, and the pid check (8) would be the only guard; it does not reach a backend in a network namespace of its own; and its one advantage, nothing on disk after a crash, is what `PrivateTmp` gives already. The socket lives for the start only, so a backend's later `sd_notify` calls, such as `STOPPING=1`, fail with `ENOENT`, which sd_notify(3) clients are expected to ignore.
6. **Ready is the arrival of `READY=1`, and `checkEndpoint` is then asked once.** The check is one GET of `checkEndpoint` through the model's proxy, and anything but 200 fails the start: `READY=1, but http://127.0.0.1:5157/health answered 503`, or, for a check that got no answer, `READY=1, but GET http://127.0.0.1:5157/health failed: dial tcp 127.0.0.1:5157: connect: connection refused`. It is not a fallback to polling, which would bring the delay back. It turns a backend that breaks the contract into a failed start that names the break, where without it the waiting requests would be forwarded to a server that refuses them, and each client would get its own error. It costs 0.3 to 0.7 ms at p50 (Measured). `checkEndpoint: none` takes `READY=1` alone. `ReadySince`, which the UI and the stats read, is `READY=1`'s arrival, not the check's end.
7. **The timeout is `healthCheckTimeout`, and an exit before `READY=1` fails the start.** The timer runs from the process start; polling's runs from after its 250 ms wait. A start that times out is killed, as a polled one is, with `no READY=1 within 10m0s (last STATUS="loading weights")`. A process that exits first fails the start at its exit, as a premature exit does under polling, with `upstream command exited prematurely, before READY=1 (last STATUS="out of memory", ERRNO=12)`.
8. **Only the process InferMux started and its descendants may send.** The socket has `SO_PASSCRED`, so the kernel attaches each sender's pid, uid and gid, which a sender cannot forge without `CAP_SYS_ADMIN`. The pid must be the started process's, or reach it up the parent chain in `/proc/<pid>/stat` within 64 steps. A thread of the started process sends with its pid, since the credentials carry the thread group's. systemd's `NotifyAccess=main` takes the main pid alone, and `NotifyAccess=all` any process in the unit's cgroup. A model has no cgroup of its own here, so the process tree is the nearest thing to `all`. `main` alone would refuse a backend started through a wrapper that forks: `cmd/vllm-wrapper`, `uv run`, a shell script without `exec`. The 0700 directory keeps other users out, root aside; the pid check is for the user's own other processes and for another model's backend. A message from outside the tree is logged at warn (`<model> notify: ignored a message: pid 4242 is not pid 4100 or its descendant`) and the wait goes on. A uid check is not done: every process of the user passes it, so it adds nothing to the directory's mode. The costs: a descendant that exited before InferMux read its message cannot be placed and is refused, the race systemd's `sd_notify_barrier` exists for; and a daemon that double-forks away from its parent is refused.
9. **What is honoured.** `READY=1` makes the model ready. `STATUS=` is logged at debug and the last one goes into the error of a start that fails; `ERRNO=` goes into that error too. `EXTEND_TIMEOUT_USEC=` is logged at debug as ignored: `healthCheckTimeout` alone is the timeout. It is global in llama-swap's config (strix has 600 s), so a model with a slow load already raises it for every model, and honouring the extension would let a backend move its own deadline; no backend here sends it, so it waits until one does. Every other assignment (`MAINPID=`, `WATCHDOG=1`, `RELOADING=1`, `STOPPING=1`, `FDSTORE=1`, `BARRIER=1`) is ignored. A datagram over 4096 bytes, systemd's own limit, is refused. The control buffer has room for the credentials only, so file descriptors a sender attaches, as `sd_notify_barrier` does, get `MSG_CTRUNC` and are closed by the kernel rather than installed: none stays in flight against the user's per-uid limit, and the barrier's wait ends.
10. **The stats say how long a request waited for its model, and how that was learnt** (0014). A request that arrived before its model was ready gets `ready_ms`, from its arrival to `ReadySince`, and `ready_by`: `notify` for `READY=1`'s moment, `health` for the poll that saw 200, which is up to 1 s after the backend was ready, and `start` for `checkEndpoint: none`. A request to a model that was already ready gets neither. They are read when the request ends, through `Server.ModelReady`, since a model is not unloaded under a request in flight.
11. **Linux only.** The socket is in `notify_linux.go`. On other platforms, which upstream llama-swap still builds for, a model with `readiness: notify` fails to start with `metadata.readiness: notify needs Linux`, and every other model is unchanged.

## The contract with a backend

What a backend has to do to be started with `metadata: {readiness: notify}`:

- Read `NOTIFY_SOCKET`. InferMux sets an absolute path. A backend written against sd_notify(3) also takes a leading `@` as an abstract name; InferMux never sends one.
- Send `READY=1` once, when a request forwarded at that moment would be served without waiting: the model loaded and the listener accepting, after the backend's own `checkEndpoint` would answer 200, since InferMux asks it once and fails the start on anything else.
- Send from the process InferMux started, any of its threads, or a descendant that is still alive when InferMux reads the message.
- Optionally send `STATUS=<text>` during the load and `ERRNO=<n>` on a failure, before exiting. The last of each goes into the error of a start that fails.
- One datagram is at most 4096 bytes. File descriptors are dropped.
- Exit on a failed load. The start then fails at the exit, not at `healthCheckTimeout`.
- After the start the socket is gone: a later send fails with `ENOENT`, which must not stop the backend. Without the opt-in `NOTIFY_SOCKET` is not InferMux's, and is whatever InferMux inherited, usually unset.

dikt-server sends `READY=1` from a thread of its main process after `/health` turns 200 (`~/Projects/dikt`, `crates/dikt-server/src/notify.rs`, contract in its `docs/server-api.md`, "Readiness: `READY=1`"). It measured 656 to 1018 ms from its start to `READY=1` with its two models in the page cache (1732 ms once), with `/health` at 200 at every `READY=1`.

## Out

- **Faster polling.** Rejected by the user (Context). At 50 ms the delay would be 0 to 50 ms after the first 250 ms, still there, for 200 requests during a 10 s load.
- **A `/health` that the backend holds open until it is ready.** InferMux would need no change: a model's `timeouts.responseHeader` is 0 unless set, so the first poll would wait for the answer and see 200 the moment it came. Rejected for what it leaves: the 250 ms wait before that first poll stays, so a 100 ms load is still seen 150 ms late; the poll's request has no deadline of its own (`healthCheckTimeout` is checked between polls), so a backend that hangs while it loads holds the start forever; and it is a protocol of InferMux's own that every other client of the backend's `/health` would also get, where sd_notify is one that systemd already speaks, so the same dikt-server runs as a `Type=notify` unit unchanged.
- **Socket handoff (`LISTEN_FDS`).** InferMux would bind each backend's port and hand the listening socket over, so a request could be forwarded before the backend accepts, and would wait in the kernel's backlog. Deferred, not rejected: it moves the wait from InferMux into the backlog, but says nothing about when the model is loaded, so the warden's `starting` and the stats' `ready_ms` would go blind. A request queued in the backlog of a backend whose load then fails gets a reset rather than an error. llama-server and dikt-server take no `LISTEN_FDS`, and a backend that does still needs readiness for the state machine. Worth revisiting only if the 1.3 to 1.6 ms that remain (Measured) ever matter.
- **Health polling for backends that do not opt in**, such as llama-server, stays as upstream has it (2). A patch to llama.cpp's server to send `READY=1` is not made (Consequences).

## Measured

Method, 2026-10-07 and 2026-10-08, on strix (Ryzen 9 9900X, 24 threads), loopback, at a load average of 40 to 48 from other agents' builds. The run reported is the one from 02:00 to 02:13 on 2026-10-08. Scripts and raw results are in `~/Projects/scratch/infermux-notify-measure/`.

- A private InferMux on :5150 with its own config (`run/config.yaml`), its models on 5151 to 5159, `host: notifytest`, policy disabled, no keys. Main at `7972d54` (before) and this branch at `59366ac` (after), each built with `go build` from `git archive` of its commit, run one after the other. Production on :5001 was not touched.
- `fakebackend` listens at once, answers `/health` 503 until its load time has passed since its own start, then 200. Polled, it stamps that moment as ready; with `-mode notify` it stamps it and sends `READY=1`. It stamps the first byte of the forwarded `POST /v1/chat/completions` at the first read after its last write on that connection, and appends both, from its own clock, to a log. llama-swap's capability probe, which goes straight to a backend the moment it is ready, is a GET and is not counted.
- `driver` unloads every model through `/api/models/unload`, waits until `/running` lists none, sends one chat completion, reads the backend's new log line, and sleeps 100 ms; 50 starts per model. The gap is the first forwarded byte minus ready. The client total is from the request's send to its answer, load included. Percentiles are nearest rank.
- `poll-*` are polled models, `notify-*` have `readiness: notify` and `checkEndpoint: /health`, `notifynone-*` have `readiness: notify` and `checkEndpoint: none`. Loads of 100, 500 and 2000 ms.

The gap, ready to the forwarded request's first byte, in ms, p50 / p99 of 50 starts:

| load | before: polled, main | after: polled | after: `readiness: notify` | after: notify, `checkEndpoint: none` |
|---|---|---|---|---|
| 100 ms | 149.1 / 152.1 | 149.1 / 153.0 | 1.51 / 2.96 | 0.82 / 3.41 |
| 500 ms | 752.7 / 755.3 | 752.6 / 756.5 | 1.58 / 4.11 | 0.98 / 2.53 |
| 2000 ms | 253.3 / 258.1 | 252.8 / 256.8 | 1.33 / 4.08 | 0.99 / 3.31 |

The client's total, from sending the request to its answer, load included, in ms, p50 / p99:

| load | before: polled, main | after: polled | after: `readiness: notify` | after: notify, `checkEndpoint: none` |
|---|---|---|---|---|
| 100 ms | 254.1 / 257.9 | 254.2 / 259.4 | 106.7 / 110.3 | 105.5 / 108.2 |
| 500 ms | 1257.4 / 1264.7 | 1257.7 / 1260.9 | 507.5 / 518.9 | 506.2 / 508.8 |
| 2000 ms | 2261.3 / 2267.2 | 2260.1 / 2264.5 | 2014.4 / 2033.0 | 2014.5 / 2024.0 |

None of these 600 starts failed. In an earlier run (`aa9cfef`, before the check kept its error, 01:45 to 01:56), one start of `notify-500` in 86 notify starts failed with `READY=1, but http://127.0.0.1:5153/health answered 502`; the proxy logs a health check's error only at debug, so its cause was lost. 450 more starts at debug level did not repeat it, and the check now keeps the error (6). An even earlier run on 2026-10-07, of `9f81823` with the close and the stats uncommitted, had the same p50s within 1 ms and p99s of 8 to 22 ms.

Before, the gap is the poll grid: 250 - 100 = 150 ms, 1250 - 500 = 750 ms and 2250 - 2000 = 250 ms, plus about 3 ms. After, it is 1.3 to 1.6 ms at p50 whatever the load, and the client's total is the load plus 6.7, 7.5 and 14.4 ms. A polled model on this branch has the same gap as on main, since polling is upstream's code unchanged.

The check after `READY=1` (6) is the difference between `notify-*` and `notifynone-*`: 0.69, 0.60 and 0.34 ms at p50.

Where the remaining millisecond goes, from a build with timestamps added at each step (not committed; `run/infermux-instr*.out`, 2026-10-07): `READY=1` is read 0.07 to 0.18 ms after the backend sent it, and the run loop marked the process ready 0.31 ms after that, 0.12 to 0.2 ms of it the socket's close waiting for its reader to wake. The close now runs in a goroutine of its own, which leaves about 0.1 ms; that figure is the same timestamps less the close, not measured again. The forwarded request's first byte comes 0.6 to 1.4 ms after `READY=1` was read. The rest is llama-swap's router: the swap's end goes through its run loop and the waiting request is scheduled and granted before it is proxied.

A model that is already loaded runs none of this: `EnsureReady` answers from the run loop's state at `StateReady` and `doStart`, where the socket lives, is not reached. What every request on this branch pays, for every model, is the stats' `ModelReady` (10): `BenchmarkModelReady`, over 11 models with one ready, took 555 to 848 ns per call over 6 runs, with 2 allocations of 528 B in all, most of it `RunningStatus`'s map.

End to end, main and this branch ran side by side (`run/loaded.sh`: main on :5150, this branch on :5155, two models each), each kept loaded and warmed with 200 requests, then 1 000 chat completions timed at the client, 5 repetitions in alternating order. The p50 round trip, in µs:

| | main, polled model | branch, polled model | main, notify model | branch, notify model |
|---|---|---|---|---|
| median of 5 | 464 | 495 | 500 | 489 |
| range | 450 to 513 | 491 to 544 | 455 to 553 | 480 to 501 |

The paired difference, branch minus main, is +41 µs [-22, +68] for the polled model and -17 µs [-64, +28] for the notify one, whose path on a loaded model is the same code. Main alone moved 63 µs between repetitions, so at this load a loaded request's cost is not resolved end to end below about 60 µs; the benchmark's 0.6 to 0.8 µs is the bound that holds.

## Consequences

- A backend whose `READY=1` is lost waits for `healthCheckTimeout`, 600 s on strix, with the requests behind it, and there is no polling to fall back on. That happens when the send fails at the backend, for one that cannot reach the path, such as a backend in a mount namespace of its own. dikt-server logs a failed send and goes on serving, on the reading that InferMux would still see `/health` turn 200; under `readiness: notify` it would not. Not decided.
- A backend in a container started by `docker run` cannot use it: the server is the container's process, a child of the container runtime, not of InferMux, so its pid is refused, and the path is not in its mount namespace unless bound in.
- llama-server and ComfyUI keep the poll's delay. llama-server sends no `READY=1`; a patch to it is not made.
- Without the opt-in, a model's environment is as before: a `NOTIFY_SOCKET` that InferMux itself inherited passes through to it, as upstream has it. The unit on strix is not `Type=notify`, so it has none.
- `readiness: notify` is in `/v1/models`, under the model's `meta.llamaswap` (4).
- The stats' `ready_ms` for a polled model is the poll's moment, not the backend's, so `ready_by` is there to tell the two apart. The UI does not show either yet.
- The deadline of a notify start is 250 ms shorter than a polled one's (7).
