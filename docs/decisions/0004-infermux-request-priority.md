# 0004. InferMux: the warden becomes the router, and the user's own prompt has priority

- Date: 2026-10-03
- Status: accepted; narrowed by 0009 (only this host's models are gated)
- Rule: InferMux is llama-swap with the warden built in, one binary on :5001. A request presenting a batch API key is refused while paused and cancelled on a yield. Every other request is interactive and is never killed: the models are not unloaded while one is in flight or was within `interactive_recent_seconds` (600). The compositor and Electron's GPU process are not contention.
- Keeps 0001 whole (decide, push, a pause is a message and not a lease) and 0002's transition-only unload, which may now be owed for a while before it is paid. Supersedes 0003: one router runs both runtimes.

## Context

On 2026-10-02 a prompt the user sent from T3 Code was killed mid-reply. The warden logged three pauses that evening at 30%, 36% and 38% "foreign" GPU load, each naming `electron, cosmic-comp`. The `electron` GPU process on this desktop is T3 Code's own (`--user-data-dir=.../T3 Code (Alpha)`, seen in NVML's process list on 2026-10-03). T3 drawing the streamed reply, plus the compositor, crossed the 25% threshold, and the warden unloaded the model that was writing the reply.

Two things were wrong. The measurement counted the side effect of the user's own interaction as somebody else's work. And the warden had no notion of whose work the model was doing. Episteme's scheduled jobs can wait, or not happen at all. A prompt the user just sent cannot.

The warden could not tell the two apart because it never saw a request. It sat beside the llama.cpp router and talked to it over HTTP. 0003 had also left an open limit: two routers (mainline on :5001, PrismML's Bonsai on :5004) could each load a model at once.

## Decision

The user's choices, 2026-10-03:

1. **The project becomes the router, built on llama-swap.** The user wanted something off the shelf for the proxy and wanted it in one binary, kept mergeable with upstream. llama-swap ([mostlygeek/llama-swap](https://github.com/mostlygeek/llama-swap), MIT) already proxies `/v1/chat/completions`, `/v1/responses` (T3's Codex) and `/v1/messages` (T3's Claude), starts one process per model on demand, and swaps them one at a time. It keeps everything under `internal/`, which Go forbids importing from another module, so it cannot be used as a library. **Its history is merged into this repository** (`git merge --allow-unrelated-histories v262`). Updating is `git fetch upstream && git merge <tag>`. The warden lives in `internal/warden/`, and llama-swap's own files carry one flag and one call in `llama-swap.go`. Rejected: a warden sidecar reading llama-swap's `/api/events` (the user asked for one binary), the patch applied in nix (no `go build` from a checkout), and a separate fork repository (splits the decisions from the code).
2. **Priority by API key, unmarked is interactive.** `batch_api_keys` in the warden's file. Read from `Authorization: Bearer`, `x-api-key` or HTTP Basic, the three ways llama-swap accepts a key. A client nobody configured is interactive, so forgetting the key makes a batch client too polite, never too rude. Per request, not per client: Episteme's chat role is the user waiting on an answer, while its pipeline is batch. The keys are labels, not secrets. InferMux listens on loopback and does no authentication.
3. **Never unload while interactive is recent.** On a yield, batch requests in flight are cancelled at once. The unload is owed from the transition and paid on the first tick where no interactive request is in flight and none ended within the last 600 s. A resume forgives an owed unload. Ten minutes covers reading a reply and typing the next prompt. The rejected alternative was to unload as soon as the last interactive request ended, which would reload the model between every turn of a conversation during a game.
4. **Batch requests get 503 while paused**, with `Retry-After: resume_quiet_seconds` and an OpenAI-style error body. The push to consumers stays: the 503 catches a batch client that missed or ignores the push. The gate and the registration of a request share one lock with the yield, so a batch request is either refused or registered before the yield cancels it.
5. **Desktop processes are excluded by name.** `desktop_processes` (`cosmic-comp`, `electron` on this host) are neither ours nor foreign, and are reported as `desktop_gpu_percent`. The user chose this over discounting all foreign load while an interactive request runs.
6. **The project is renamed InferMux.** It routes models, frees ComfyUI, and is meant to cover runtimes beyond llama.cpp, such as onnxruntime. The policy component keeps the name warden.

`internal/warden` imports nothing from llama-swap. It reaches the models through a two-method interface (`Running`, `UnloadAll`) that `infermux.go` implements over whichever server is active, because a config reload replaces llama-swap's server but not the warden. The accessors on the server are in `internal/server/warden.go`, a file of their own, so an upstream rename fails the build instead of a merge.

The warden's configuration is its own YAML file (`-warden-config`), not a section of llama-swap's. llama-swap decodes its file leniently today, but a stricter decoder upstream would turn a merge into a startup failure. Without the flag InferMux is plain llama-swap.

`/warden/verdict` and `/warden/resources` replace `:5003/verdict` and `:5003/resources`. `/status` is gone: llama-swap's `/running` lists the models.

## What the user gives up, and the open items

- **An interactive prompt during a game loads a model.** That is the point of the priority, and it means the game shares the card with the model for as long as the conversation lasts plus ten minutes. With `fit = on` llama.cpp fits what it can into what is free, so the model runs slower rather than failing, but a game that allocates after the model may not get its memory. Not measured.
- **Excluding `electron` by name excludes every Electron app**, Bitwarden and Tabby included, and a game launcher built on Electron. Firefox stays counted. A rule keyed on T3's user-data directory would be narrower; it was not chosen.
- **A pause is still a message, not a lease** (0001). A warden that dies while Episteme is paused leaves it paused. Now the warden also dies with the router, since they are one process, and systemd restarts both.
- **A model generating squeezes the foreign share below the threshold.** NVML reports time-slice shares. While our model generated at 88 to 99%, `glmark2-gbm` started beside it got 8 to 12%, under `gpu_busy_percent` (25). A game started during a long generation may not trigger a yield until the generation ends. Measured 2026-10-03, not decided. Candidates: compare the foreign share against what our processes leave (`foreign / (100 - ours)`), lower the threshold while ours is busy, or count a foreign process's presence and VRAM rather than its share.
- **Tailcat requests bypass the warden.** llama-swap's optional Tailcat listener has its own handler, which `startWarden` does not wrap. Tailcat is not enabled here.
- **Consumers had to change.** Episteme called llama.cpp router-mode endpoints at the server root: `POST /models/unload` and `GET /props`, and read `status.value` and `status.args` from `/v1/models`. llama-swap has none of these (its equivalents are `POST /api/models/unload` and `GET /running`). Episteme's unload is best effort and becomes a no-op; its bench breaks. Episteme does not send a batch key yet, so until it does, its pipeline is interactive and is stopped only by the push. The user chose to list these changes and leave Episteme alone in this change.

## Measurement

Live on 2026-10-03, after the NixOS switch:

- The probe ran under the service sandbox (`DynamicUser`, no `ProtectProc`): NVML listed every process and their cgroups were readable. Before this, the sandboxed probe had never been watched running (CLAUDE-TODO).
- A 9B model started by InferMux appeared as `llama-server` in `infermux.service` and was counted as ours (33%, 6178 MB). T3's Electron GPU process and cosmic-comp were reported as desktop.
- Foreign load from `glmark2-gbm` on the RTX 3080 (67%) paused the verdict and logged `Unload deferred: interactive request in flight or 19s ago`. The model stayed loaded, a batch request got 503 with `Retry-After: 300`, an interactive request during the pause got 200, and Episteme took the pause.
- A ComfyUI job in the queue paused the verdict with no interactive request in the last ten minutes. The models unloaded on that tick, and two batch requests in flight were cancelled.
- Those two cancelled requests first reached the client as HTTP 200 with an empty body: llama-swap writes nothing once the request context is cancelled. `Wrap` now cancels with a cause of its own and answers 503 with `Retry-After` when nothing was written yet, and aborts the connection when a stream had started. After the fix both got `503` with `batch request cancelled, the GPU was yielded: ComfyUI has 1 job queued`.
- The verdict resumed on its own at 10:04:37, 300 s after the glmark2 load ended, and Episteme took `resume`.
