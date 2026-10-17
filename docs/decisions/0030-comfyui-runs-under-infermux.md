# 0030. ComfyUI runs under InferMux, as a model beside the GPU's swap

- Date: 2026-10-17
- Status: accepted; built 2026-10-17 on the branch `comfyui-backend` with nixcfg's of the same name, not deployed (Verification)
- Amends [0002](0002-linux-nvml-router-unload-comfyui.md) (ComfyUI's lifecycle is InferMux's; its work stays contention, and a yield frees it and never stops it), [0004](0004-infermux-request-priority.md) and [0017](0017-a-priority-process-takes-the-card.md) (a priority process stops ComfyUI under a render), [0006](0006-two-hosts-keys-and-the-cloud.md) (the ComfyUI listener takes no key), [0028](0028-a-start-on-the-gpu-needs-room-on-the-card.md) and [0029](0029-a-group-stays-loaded-together-on-a-device-that-swaps.md) (ComfyUI's start takes no start slot and no fit check, and every other start on the GPU frees it first).

## Context

The user's rule of 2026-10-17 14:21 CEST: InferMux is the only program allowed to host a model. ComfyUI broke it. `comfyui.service` started at boot on 127.0.0.1:8188, loaded its checkpoints, LoRAs and VAEs onto the 3080 on its own, and `https://comfyui.java-tint.ts.net` (Tailscale Service `svc:comfyui`) reached it with no key. The warden only watched it (0002): a job in `/queue` yielded the card, and `/free` went out after 600 s of empty queue. A job could start loading up to 5 s before the warden's next tick saw it (0002's race).

The user's request, 14:23 CEST: "make comfyUI go through infermux somehow, while still keeping the power of comfyui model management." ComfyUI's node graph keeps picking the models, and its smart memory, dynamic VRAM and offload stay. InferMux starts it on demand, counts what it holds, makes it give memory back for a game or another model, and fronts its API.

What existed:

- llama-swap 262, the version merged, has a ComfyUI endpoint: a model named `comfyui_auto` is served at `/comfyui/` (upstream's guide `docs/kb/guides/upstreams/comfyui.md`). llama-swap forces `ignoreWebsockets` and a `concurrencyLimit` of 999 on it, and a `GET` of a static asset, `/ws` or `/api/jobs` cannot start it, so an open tab does not start it again after a stop.
- A model's process may be anything with an HTTP port: the reranker is a transformers server (`${semsearch-rerank}`), immich-ml is Immich's own.
- Measured on strix, 2026-10-17: the idle ComfyUI (after a `/free`) holds 216 MiB in NVML's process list. It started in 14.5 s on 2026-10-15 (journal, unit start 21:57:39.47 to `Starting server` 21:57:53.97). ComfyUI 0.39 runs dynamic VRAM (comfy-aimdo) by default.
- `/free` answers 200 at once and only sets a flag. ComfyUI's worker thread acts on it after the job it is running, or at once when idle (`main.py`, `prompt_worker`). An unload has to be confirmed from NVML, not from the answer.
- A swap in llama-swap waits for the evicted model's HTTP requests in flight. A ComfyUI render is not one: `POST /prompt` returns as soon as the job is queued, and the progress comes over the websocket, which llama-swap ignores. A swap would kill a render.

## Decision

The user, 2026-10-17, answering the agent's four forks (the options not taken are under Rejected).

1. **ComfyUI stays up beside whatever the GPU swaps, and every other start on the GPU frees it first.** The user picked "Beside, free on start". A new device key, `beside: [comfyui_auto]`, puts the model in every matrix set of a device that swaps, so a swap never stops it and its start stops nothing. Before any other model starts on the GPU, ComfyUI gets `/free` while its queue is empty, and the start waits for NVML to show the VRAM back. A render in progress is left alone, and the start goes into what is left, as a chat request beside a render did before.
2. **ComfyUI keeps its own unit.** The user picked "Its own unit". `comfyui.service` keeps the `comfyui` user, its sandbox, the Manager fix in its `preStart`, and its logs, and no longer starts at boot. The model's command is a script that runs `systemctl start comfyui.service`, waits while the unit is up, and stops it on SIGTERM. A polkit rule lets InferMux's user start and stop that one unit, and the unit `BindsTo=infermux.service`, so a daemon that stops or dies takes ComfyUI with it.
3. **The browser opens the Tailscale Service URL, with no login.** The user's words, relayed by the aide: "no login. use the ts service url". `https://comfyui.java-tint.ts.net` stays as it was. How InferMux sits behind it was left to the agent, which picked a second listener: InferMux takes 127.0.0.1:8188 with `-comfyui-listen`, serving `/comfyui/` at its root with no key, and ComfyUI moves to 127.0.0.1:8190. The frontend sees no path prefix.
4. **A game launch and a priority process stop a render; load alone never does.** The user picked "Hold + priority only", with the reason, relayed by the aide: foreign load must not kill a render, because NVML's time-share reads about 4x high at idle clocks (the wallpaper spiked to 25% about once in 110 samples on 2026-10-17, measured by the aide's probe in `~/Projects/scratch/wallpaper-gpu/`). `infermux-hold` and a 0017 priority process send `/interrupt`, clear the queue and stop ComfyUI. A game seen only by its load lets the render finish, and ComfyUI gets `/free` once its queue is empty. The user widened the first half the same day: `infermux-hold` always wins over every client, which is 0024, item 4, amended.

### How it works

The agent's calls, made in the proposal the user answered or while building it.

1. **The model.** `comfyui_auto` is in the one config, on `strix/gpu`, `unlisted`, `ttl: 0`, with `beside: [comfyui_auto]` on that device. The derivation adds each `beside` model to every set of its device's swap, `together` groups included. A `beside` model in a `together` group, on a concurrent device, or not a model is refused, and one not placed on its device is warned about (`internal/placement`).
2. **The free before another start** (`Fits`, `freeComfyUIFor`). It runs after the start takes the GPU's start slot (0029) and before the fit check (0028) reads the card. With `comfyui_unit` set, `/free` goes out only while ComfyUI's processes hold more than 512 MB, and the card is read every 250 ms until they hold 512 MB or less, or hold the same twice after giving some back, for up to 10 s. Without `comfyui_unit` nothing can read ComfyUI's VRAM, so `/free` goes out and nothing waits. ComfyUI's own start takes no slot and no fit check: it loads no weights until a job asks for them.
3. **A job through InferMux** (`beforeComfyUIJob`). A `POST /comfyui/prompt` or `/comfyui/api/prompt` counts as a queued job at once, and the warden ticks before the job reaches ComfyUI, so the consumers are paused and the batch work cancelled before ComfyUI loads anything. That ends 0002's race for a job that comes through InferMux. Then the GPU's other models stop, unless an interactive request is in flight on one (0004). `interactive_recent_seconds` does not hold this back: a render is the user's statement that the card is wanted, as a game launch is in 0024.
4. **Contention apart from ComfyUI's own** (`OthersContend`). The tick reads the card without ComfyUI's queue, its processes' load, and its VRAM, counted as free. When that still contends, ComfyUI gets `/free` as soon as its queue is empty, without waiting out `comfyui_idle_seconds`. Without other contention the idle window still applies.
5. **A yield never stops ComfyUI.** The yield's unload stops the device's other models (`unloadOn`). The verdict keeps 0002's view: a ComfyUI job is contention, and `comfyui_auto` is no loaded model for the free VRAM rule, so a cache ComfyUI keeps after a render still pauses the consumers until its `/free`.
6. **What stops it.** A priority process's yield (`stopComfyUI`), at the transition or when the priority process arrives during a pause, and the by-hand `/warden/unload` that `infermux-hold` calls, clear ComfyUI's queue (`POST /queue {"clear": true}`), interrupt its job (`POST /interrupt`), then stop the model. The off file stops it at once with the other models (0025), with no clear first. A config reload waits for ComfyUI's jobs while it runs as a model, since the reload stops it, and a render is no HTTP request in flight.
7. **Its requests are not interactive activity on the GPU.** A ComfyUI request in flight counts toward a reload's wait, never toward `interactive_recent_seconds` or the unload's quiet. Its pages, files and websocket pass untracked, so an open tab and a render's progress frames hold nothing back. The key's allow list covers every `/comfyui/` request, pages too, since its outputs are files.
8. **What it holds is counted where NVML shows it.** ComfyUI's processes stay in `comfyui.service`'s cgroup, so the probe names them by `comfyui_unit` as before, and they are not ours. `/warden/resources` reports their load and VRAM as `comfyui_gpu_percent` and `comfyui_vram_mb`, inside the foreign figures, and ComfyUI's queue. The fit check reads the card's own free figure, so whatever ComfyUI holds counts against every start.
9. **The listener** (`infermux_comfyui.go`). `-comfyui-listen` opens only on a loopback address. It puts `/comfyui` in front of every path, so it reaches ComfyUI and nothing else: `/warden/verdict` there is ComfyUI's `/warden/verdict`. It serves the main port's chain, the drain and failover included, and the warden gives its requests an interactive key named `comfyui-listener`, allowed only `<host>/comfyui_auto`. On the main port ComfyUI is a model like the others, behind a key. The browser's writes pass the guard's same-origin rule with `comfyui.java-tint.ts.net` in `trusted_hosts`.
10. **A host with no `comfyui_auto`**, the zbox, answers `/comfyui/` with llama-swap's 404 and stops no model on the way.

## Rejected

1. Freeing ComfyUI only when the starting model's `vram_mb` does not fit: ComfyUI would keep its cache next to the reranker, and a chat model, which has no `vram_mb`, would load with `-fit` into what is left and run slower. Also rejected: `comfyui_auto` as one more model in the GPU's swap, upstream's way. Any other GPU model would stop ComfyUI's process, so every switch back costs the 14.5 s start and an open tab loses its connection, and a swap would kill a render.
2. ComfyUI as a child of the daemon. No polkit rule, but ComfyUI would run as InferMux's dynamic user in its sandbox: its files in `/var/lib/comfyui` and `/srv/models/comfyui` would get another owner, `MemoryDenyWriteExecute` may stop custom nodes that compile kernels, and it would count as ours by cgroup.
3. A login, an interactive key `comfyui` asked for with `WWW-Authenticate: Basic` and saved by Firefox, which the agent had proposed. Also `https://strix.java-tint.ts.net:5001/comfyui/` with no second listener: the frontend would run under a path prefix, which upstream supports and which nobody had tried here.
4. A render that is never stopped, `infermux-hold` refusing the launch while ComfyUI has a job, which the agent had recommended. Also a render stopped for any yield: NVML's load at idle clocks would kill renders for a wallpaper.

## Consequences

- ComfyUI is not running after boot. The first page load or job starts it, about 15 s.
- A chat request during an idle stretch drops ComfyUI's loaded models at once, not after 10 minutes. The next render loads them again from RAM.
- A tab left open after ComfyUI stopped shows "reconnecting" until something on the page starts it (upstream's 409 for the websocket).
- Anything on strix can reach ComfyUI without a key, on 127.0.0.1:8188 through the listener as before, and on 127.0.0.1:8190 directly, which skips the job hook. Both are loopback only, as 8188 was.
- The job hook's tick announces the verdict to the consumers before the job goes on, so a consumer that is slow to answer delays the job by up to its timeout.
- A deploy stops a running render: 0026's drain waits 40 s for HTTP requests, and a render is not one.
- A job a batch key sends is refused during a yield, like any batch request. One already running is not interrupted on a yield; that would need `/interrupt` and the key of each prompt.
- ComfyUI Manager's restart button ends ComfyUI's process and systemd starts it again. The model's script follows the unit while it is starting, so llama-swap reads the model as running throughout.
- `beside` is a new config key, so both hosts' daemons must run this build before a config that has it is published.

## Verification

Tests, each seen to fail with its change taken out (33 mutations, each failed a test):

- `internal/placement`: `TestABesideModelIsInEverySet`, `TestABesideModelAloneOnItsDevice`, `TestBesideRefusedAndWarned`.
- `internal/warden`, `comfyui_model_test.go`: the free before a start and its wait, a busy or empty ComfyUI left alone and its start taking no slot, the job hook's yield and unload and the request in flight it keeps, a yield that never stops ComfyUI, the free VRAM rule, the free once others contend, the priority stop, the by-hand unload's clear, the reload's wait, the keys and the listener, and a host with no `comfyui_auto`. `probe_test.go` splits ComfyUI's load and VRAM out of the foreign figures.
- The root package: `TestComfyUIsListenerReachesComfyUIAloneAndWithoutAKey` (no key, the frontend's paths, an escaped slash, `/warden/` and `/v1/` reaching ComfyUI alone, the key and allow list on the main port) and `TestComfyUIsListenerRefusesAnAddressOffLoopback`.

Live on 2026-10-17 on strix, against a second daemon built from b436c9e in a user unit on 127.0.0.1:5191, its listener on 5188, and ComfyUI 0.39.0 in a transient user unit on 8190 started by a copy of nixcfg's `comfyui-model` script, beside the host's own daemon and ComfyUI (`~/Projects/scratch/comfyui-live`, `hold-check.sh` and the files it reads):

- A workflow sent with no key to the listener's `/api/prompt` started `comfyui_auto` and was answered in 9.3 s, ComfyUI's start included. It loaded the LTX 2.5 VAE and encoded one 256x256 image, history `success`, and NVML then read ComfyUI's process at 1688 MiB.
- A chat to Qwen 3.5 4B on 5191 then started beside it. The log has "ComfyUI gave back 1430 of 1688 MB for <qwen> in 0.5s" before Qwen's health check, NVML read ComfyUI at 258 MiB 0.35 s after the request was sent, and the chat answered 200 in 3.07 s.
- `infermux-hold` on the second daemon's control socket, with one ComfyUI job running and five pending, a streaming chat 2.4 s in and a plain one waiting behind it: the plain chat got 503 `gpu_held` with `Retry-After: 300`, the stream ended with no `[DONE]`, ComfyUI logged a global interrupt 6 ms after the cut and its unit stopped 113 ms later, and the command started 1.34 s after `infermux-hold` did, with neither Qwen nor ComfyUI in NVML's list. The verdict went back to auto when the command exited. llama-swap's access log records the plain chat as 499, its status for a cancelled context, while the client got the 503.
- Through the listener, a write with Host `comfyui.java-tint.ts.net` and that Origin answered 200, and one with a foreign Origin got 403 from InferMux's guard. The host's ComfyUI, reached through `https://comfyui.java-tint.ts.net` with that Origin, answered a POST 400 `no_prompt`, past its origin check, and 403 on loopback, so Tailscale's serve passes the browser's Host on.

Not yet watched: the deployed `comfyui.service` started and stopped through polkit by the daemon's dynamic user, a browser through Tailscale reaching the listener, and a priority process stopping a render.
