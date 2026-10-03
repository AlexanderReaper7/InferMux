# CLAUDE-TODO.md

Claude's working list for InferMux: what is built and shipping but has not been *watched running*. The point is to stop a claim that something works when all there is is code that exists and tests that pass. Move an item out when it has been observed, and say in the commit what was observed.

## Watched so far on Linux

- 2026-09-28: the probe against the real card, processes named and attributed to their cgroup units, free VRAM read. Chromium's argv rewrite was found this way and fixed.
- 2026-10-03, InferMux under its systemd sandbox: NVML listed every process from the `DynamicUser` service and their cgroups were readable. A model started by InferMux was counted as ours by its `infermux.service` cgroup. T3's Electron GPU process and cosmic-comp were reported as desktop.
- 2026-10-03, a yield: foreign load from `glmark2-gbm` paused the verdict, the unload was deferred under a recent interactive request, a batch request got 503 with `Retry-After`, an interactive request got 200, and Episteme took the pause.
- 2026-10-03, the verdict resuming on its own after the quiet window, with Episteme taking `resume`.
- 2026-10-03, a yield to a queued ComfyUI job with no interactive request recent: the models unloaded at once, and two batch requests in flight got 503 with `Retry-After`. The first attempt returned 200 with an empty body, which is how that bug was found.

## Not yet verified live

- **An owed unload being paid**: contention lasting past `interactive_recent_seconds` after the last interactive request, and the models unloading then. Covered by `TestAnInteractiveRequestInFlightDefersTheUnload` only.
- **ComfyUI's `/free` after the idle window**, and the VRAM actually coming back.
- **The race in 0002**: what ComfyUI does when a job starts loading into a card a model has not yet let go of. 0004 widens it: an interactive request can now keep a model loaded beside a ComfyUI job on purpose.
- **A batch stream aborted mid-reply.** A yield after the first byte should drop the connection (`http.ErrAbortHandler`). Covered by `warden_test.go` only; both live cancellations happened before any byte was written.
- **A game started while a model generates** (0004, measured gap): the foreign share stayed at 8 to 12% beside our 88 to 99%, below the threshold. Not decided.
- **A game beside a model kept for an interactive session** (0004): whether the game gets its memory.
- **llama-swap's performance graphs**: its monitor looks for `nvidia-smi` on `PATH` and logs "no GPU monitoring tool available" in the sandbox. Unrelated to the warden, which reads NVML itself.
