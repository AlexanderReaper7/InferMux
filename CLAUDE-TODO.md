# CLAUDE-TODO.md

Claude's working list for InferMux: what is built and shipping but has not been *watched running*. The point is to stop a claim that something works when all there is is code that exists and tests that pass. Move an item out when it has been observed, and say in the commit what was observed.

## Watched so far on Linux

- 2026-09-28: the probe against the real card, processes named and attributed to their cgroup units, free VRAM read. Chromium's argv rewrite was found this way and fixed.
- 2026-10-03, InferMux under its systemd sandbox: NVML listed every process from the `DynamicUser` service and their cgroups were readable. A model started by InferMux was counted as ours by its `infermux.service` cgroup. T3's Electron GPU process and cosmic-comp were reported as desktop.
- 2026-10-03, a yield: foreign load from `glmark2-gbm` paused the verdict, the unload was deferred under a recent interactive request, a batch request got 503 with `Retry-After`, an interactive request got 200, and Episteme took the pause.
- 2026-10-03, the verdict resuming on its own after the quiet window, with Episteme taking `resume`.
- 2026-10-03, a yield to a queued ComfyUI job with no interactive request recent: the models unloaded at once, and two batch requests in flight got 503 with `Retry-After`. The first attempt returned 200 with an empty body, which is how that bug was found.
- 2026-10-03, the web UI and config dir (0005), after the switch to nixcfg `2cdbbfe`: the daemon loaded the six models from the bind-mounted `configDir`. A model edited through the UI's API while an interactive stream ran logged "llama-swap config reload waits: 1 interactive request(s) in flight"; the stream finished with `[DONE]` at 11:29:36.716 and the reload ran at 11:29:36.715, after the last byte was handed over. Reverting the edit left the file byte-identical. A manual resume during the warden's own pause held for 4 min 47 s and ended, logged, when the warden resumed on its own. `/warden/unload` answered 409 during a stream and unloaded after it; forgive, cancel-batch, manual pause and auto, and ComfyUI's `/free` each did what they say. Cross-origin, rebinding and header-less writes got 403, a non-loopback Host on the UI got 421. Screenshots at 2560x1300 from headless Chromium.
- 2026-10-03, the tailnet and the KV warning (0005, 8 and 9), after the switch to nixcfg `d2424de`: `tailscale serve` listed :5001 and :5010 beside T3's :443. Through `https://nixos-desktop.tail.ts.net`, from this machine, the UI answered, a same-origin write through the UI and one to the daemon got 200, another origin got 403, and `/v1/models` listed the six models. "Build now" built `unit-infermux.service` in 2 s, the same store path `/etc/systemd/system/infermux.service` points at. Setting the 9B's V cache to `q4_0` marked it in the list and in a banner (`q5_0-q4_0` has no kernel), and the earlier build showed as stale; the file was put back byte for byte.
- 2026-10-03, `e2e/run.sh` (64 checks, Chromium through Playwright, against the nix-built binaries, a fake model server and a throwaway git repo): an edit from the table reached the file and the daemon's argv; a reload waited behind an interactive stream that finished whole; a batch stream was cancelled while an interactive one beside it got all 30 chunks; pause by hand gave batch 503 and interactive 200; a commit from the Changes tab made one commit with the message and left nothing uncommitted. A daemon mutated to reload at once failed the three reload checks.

## Not yet verified live

- **The tailnet from another device (0005, 8).** Only requests from this machine to its own tailnet name were made.
- **"Build now" compiling llama.cpp (0005, 9).** Only a build with every input already in the store was run.

- **An owed unload being paid**: contention lasting past `interactive_recent_seconds` after the last interactive request, and the models unloading then. Covered by `TestAnInteractiveRequestInFlightDefersTheUnload` only.
- **ComfyUI's `/free` after the idle window**, and the VRAM actually coming back.
- **The race in 0002**: what ComfyUI does when a job starts loading into a card a model has not yet let go of. 0004 widens it: an interactive request can now keep a model loaded beside a ComfyUI job on purpose.
- **A batch stream aborted mid-reply.** A yield after the first byte should drop the connection (`http.ErrAbortHandler`). Covered by `warden_test.go` only; both live cancellations happened before any byte was written.
- **A game started while a model generates** (0004, measured gap): the foreign share stayed at 8 to 12% beside our 88 to 99%, below the threshold. Not decided.
- **A game beside a model kept for an interactive session** (0004): whether the game gets its memory.
- **llama-swap's performance graphs**: its monitor looks for `nvidia-smi` on `PATH` and logs "no GPU monitoring tool available" in the sandbox. Unrelated to the warden, which reads NVML itself.
- **The guard against a real cross-origin browser page.** Live checks used curl with forged `Origin` and `Host` headers.
