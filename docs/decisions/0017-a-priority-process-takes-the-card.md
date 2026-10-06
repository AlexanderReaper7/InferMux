# 0017. A priority process takes the card

- Date: 2026-10-06
- Status: accepted; built and tested 2026-10-06, not deployed or watched
- Rule: `priority_processes` in `warden.yaml` names processes whose CUDA work takes the card. One of them in NVML's compute list is contention at any load. The yield it causes unloads the models without waiting out `interactive_recent_seconds`, but never under a request in flight. One arriving during a pause that has already unloaded owes another unload. Requests during the yield are served as in any yield (0002, 2): an interactive one loads its model again. A name in both `desktop_processes` and `priority_processes` is refused.
- Keeps 0004, 1 (the user's own requests are never killed) and 0004, 5 (desktop processes by name).

## Context

nixcfg, 2026-10-06: OBS runs NVIDIA's AI Green Screen (Maxine Video Effects SDK) on the webcam, and Teams takes the result through OBS's virtual camera. Measured that day:

- The filter uses 6-8% SM, under `gpu_busy_percent` (25), so the warden saw no contention.
- Its TensorRT engines need about 360 MiB. With the failover embedder loaded (Octen-Embedding-4B, 5838 MiB) and the desktop at about 3.5 GB of the 3080's 10 GB, the load failed out of memory.
- The filter fails closed by the user's choice: with no matte it draws nothing, so a model holding the card makes the user disappear from the call.
- Plain OBS is in NVML's graphics list only (34 MiB). It joins the compute list (360 MiB) when the filter creates its CUDA context, and stays there until OBS exits, even with the filter disabled.

## Decision

The user, 2026-10-06, each asked in turn.

1. **The signal is a named process in NVML's compute list.** Rejected: a claim the filter POSTs to a new `/warden/claim` endpoint every 10 s, which would end the moment the filter is disabled, but needs an endpoint, an InferMux key for OBS and HTTP code in the plugin. The cost of the chosen one: an OBS left open with the filter loaded keeps the models off until it exits. NVENC recording counts too.
2. **The unload happens at once, never mid-request.** A recent interactive request does not defer it; one in flight does. Rejected: the 600 s deferral games and ComfyUI get, which would leave the user invisible in a call for up to 10 minutes after using a model.
3. **Reloads are allowed, as today.** Rejected: refusing local loads with 503 while a priority process holds the card, which the agent recommended. The cost: an interactive request during a call can load a model that starves the green screen again. The unload is not repeated for it; only a priority process arriving during a pause owes a second one.

How it is built, the agent's choice: the probe marks a process as priority when it is not ours, is in the compute list and its name is listed. The names are collected before the 10-process display cap, so a busy card cannot hide one. `IsContended` checks priority first, and the verdict carries `priority`, which the warden reads in `settleUnload` to use a window of 0. Graphics-only OBS is not priority, so OBS open for screen recording without the filter changes nothing.

## Consequences

- With the filter in a scene, OBS holds the card from its start to its exit, and models on this card stay unloaded except when a request loads one.
- The embedder fails over to this card (0016) only when the zbox cannot serve, and the `semantic-search` key is interactive, so on reaperboi such a request loads the embedder's 5838 MiB during a call. That is decision 3's cost in its most likely form.
- The Settings tab edits the list beside Desktop processes.
