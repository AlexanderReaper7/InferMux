# Decision log

Why InferMux (llama-warden until 2026-10-03) is built the way it is. One file per decision: the date, what was chosen, what was rejected, and what it was measured against.

These are records, not instructions. The *rule* a decision produced lives in [AGENTS.md](../../AGENTS.md), which is read every session; the reasoning lives here, which is read when someone asks "why is this like this" or is about to undo it. Grep this directory before changing something that looks arbitrary.

A record says what is true now. A decision that changes is edited in place and gets an `Amended by` line in its header; one replaced whole gets a `Status:` line naming its replacement and keeps its reasoning, because the reason it was wrong is the useful part. Git keeps the earlier text ([ADR 0017](https://github.com/AlexanderReaper7/agents/blob/main/docs/adr/0017-records-describe-the-current-state.md) in the agents repository).

| # | decision | date |
|---|---|---|
| [0001](0001-the-warden-decides-and-says-so.md) | The warden decides, and says so: policy leaves Episteme, and a verdict is pushed | 2026-09-13 |
| [0002](0002-linux-nvml-router-unload-comfyui.md) | Linux only: NVML, unload through the router, ComfyUI queue as contention and `/free` when idle | 2026-09-28 |
| [0003](0003-multiple-model-routers.md) | Watch and unload multiple model routers. Superseded by 0004 | 2026-10-02 |
| [0004](0004-infermux-request-priority.md) | InferMux: llama-swap merged in, batch and interactive requests, desktop processes excluded | 2026-10-03 |
| [0005](0005-web-ui-and-config-outside-nix.md) | A web UI as its own process, model and warden files outside the store, reload when quiet, manual verdict | 2026-10-03 |
| [0006](0006-two-hosts-keys-and-the-cloud.md) | Two hosts, each a front door; every client has a key; OpenRouter as a peer | 2026-10-03 |
| [0007](0007-model-settings-derived.md) | Each model's settings derived from its command and GGUF; Codex gets them as its catalog | 2026-10-03 |
| [0008](0008-the-zbox-has-its-own-ui.md) | The zbox has its own UI and nixcfg checkout; Commit can take the GPG passphrase | 2026-10-03 |
| [0009](0009-the-gate-is-for-this-hosts-card.md) | The gate is for this host's card: another host's and the cloud's requests are not paused, cancelled or counted here | 2026-10-03 |
| [0010](0010-a-peers-facts-from-its-own-list.md) | A cloud peer's facts come from the peer's own model list; T3's OpenRouter instance folds into Local Codex | 2026-10-03 |
| [0011](0011-the-cpu-embedder-is-a-peer.md) | Replaced by 0022: the CPU embedder is a peer of its own unit, not a local model the warden would stop, count or gate | 2026-10-03 |
| [0012](0012-a-model-names-its-gguf-on-hugging-face.md) | Replaced by 0022 except the naming: a model names its GGUF on Hugging Face, and the UI downloads it from main | 2026-10-03 |
| [0013](0013-immich-ml-under-the-zbox-through-an-adapter.md) | Immich ML runs under the zbox's InferMux, and an adapter gives Immich its key | 2026-10-03 |
| [0014](0014-performance-measured-at-the-front-door.md) | Each host times the requests it serves, in memory, and the UI shows both hosts | 2026-10-04 |
| [0015](0015-a-stuck-agent-is-noted-escalated-then-refused.md) | A stuck agent is noted, sent to a stronger model, then refused; the presets get Qwen's sampling | 2026-10-05 |
| [0016](0016-a-model-fails-over-between-places.md) | A model fails over between places, the one config's lanes, on 502, 503 or 504 | 2026-10-06 |
| [0017](0017-a-priority-process-takes-the-card.md) | A priority process with a CUDA context takes the card: contention at any load, unloaded at once but never under a request | 2026-10-06 |
| [0018](0018-streams-pass-through-and-a-session-is-interactive-while-data-moves.md) | Streams pass through unchanged, routed by model; a WebSocket session is interactive only while data moves, and is closed with a code before an unload | 2026-10-07 |
| [0019](0019-model-overflow-uses-destination-capacity.md) | Any model can overflow at a destination request limit; embedding work tries zbox, an idle 3080, then CPU for batch keys | 2026-10-08 |
| [0021](0021-overflow-never-takes-a-yielded-card.md) | Overflow never takes a yielded card, whatever the key; bulk indexing gets a batch key | 2026-10-09 |
| [0022](0022-one-signed-config-placed-by-device.md) | One signed config on every host, models placed on devices in balance and failover lanes, edited as YAML | 2026-10-10 |
| [0023](0023-a-batch-request-is-a-job-paused-mid-inference.md) | A batch request is a job InferMux pauses mid-inference and resumes; first-party consumers use a job envelope, nothing is pushed to them | 2026-10-10 |
| [0024](0024-a-game-clears-the-card-before-it-starts.md) | A game clears the card before it starts: `infermux-hold` pauses and unloads through a keyless control socket, and refuses the launch if the card stays busy | 2026-10-12 |
| [0025](0025-a-safety-stop-takes-the-local-models-off.md) | A safety stop takes the local models off and leaves the daemon up: every class refused and cancelled, the clouds and the other hosts passed, read from a file before the first request | 2026-10-14 |
| [0026](0026-a-deploy-waits-for-the-requests-running.md) | A deploy waits up to 40 s for the requests running and refuses new ones with 503 `draining`, while off too; the zbox controller SIGKILLs the models through pidfds | 2026-10-14 |
| [0027](0027-one-agents-md-ours-above-upstreams-guide.md) | One AGENTS.md: InferMux's rules above llama-swap's guide, kept verbatim so upstream's changes merge; CLAUDE.md is upstream's `@AGENTS.md` | 2026-10-15 |
| [0028](0028-a-start-on-the-gpu-needs-room-on-the-card.md) | A start on the GPU needs room on the card: a model's `vram_mb` against NVML's own free figure, read at the start gate; short, every class gets 503 at once | 2026-10-15 |

The records this project came from stay in Episteme's own log: 0023 (the agent holds no policy), 0024 (brake on contention, a pause has an author), 0041 (the agent applies a configuration it is handed), 0042 (one console behind a tray icon). 0001 above supersedes the first and takes half of the second.
