# 0032. The GPU may be absent

- Date: 2026-10-23
- Status: accepted; built 2026-10-23, not deployed (Verification, below)
- Amends [0004](0004-infermux-request-priority.md): another exception to "the user's own request is never killed". A start on the GPU while the host has none is refused at once, as a start the card has no room for is (0028).
- Amends [0028](0028-a-start-on-the-gpu-needs-room-on-the-card.md): a card the start gate cannot read because the host has none no longer lets the start through.
- Keeps [0002](0002-linux-nvml-router-unload-comfyui.md)'s rule that a failed probe leaves the verdict alone, and [0016](0016-a-model-fails-over-between-places.md)'s rule that a 503 fails over.

## Context

The user's request of 2026-10-23: "make my other applications understand and gracefully handle situations where the hardware is not what it expects, like when the 3080 is bound to the guest."

strix has a boot entry, `rtx3080-passthrough` (stealth-vm's `docs/passthrough.md`), that binds the 3080 to vfio-pci for the `adversary` VM and blacklists the nvidia modules. `svm gpu host` hands the card back to the host at runtime and loads the driver, and `svm gpu guest` takes it away again. NVML then answers `nvmlInit` with ERROR_DRIVER_NOT_LOADED, or with ERROR_LIBRARY_NOT_FOUND while no libnvidia-ml is in the generation.

What InferMux did in that boot, read from strix's journal and `ps` on 2026-10-23 between 02:29 and 03:06:

- The loop logged `[WARN] Warden probe failed, verdict unchanged: nvml init: Driver Not Loaded` every 5 s, 437 times in 36 minutes.
- At 02:29:41 the fit check let the GPU's place of `Octen-Embedding-4B.Q8_0` start: "needs 5900 MB of VRAM and the card could not be read, so it starts". llama-server, given `--n-gpu-layers 99`, found no CUDA device and ran on the CPU threads, in `system.slice/infermux.service` rather than `idle.slice`. At 03:06 it held 15.1 GB of RSS, beside the CPU place's own copy in `infermux-cpu.service` with 14.0 GB. Requests the zbox refused, during its thermal off file, went to the GPU's place and took 2 to 9 s each. The one config's comment on the CPU place says an interactive query should wait for a card rather than take that long.
- `/warden/verdict` showed the card as a probe that failed, which the UI draws as an error, not as a card the host does not have.

On 2026-10-22, when the card came back to the host at runtime, InferMux used it with no restart, because go-nvml calls `nvmlInit` again after a failed one and the fit check reads the card at each start. The change went unlogged: the last failed probe was at 23:36:57 and the GPU's place passed its health check at 23:37:04.

## Decision

1. **A reading that finds no GPU is a state, not a failure.** ERROR_DRIVER_NOT_LOADED and ERROR_LIBRARY_NOT_FOUND from `nvmlInit`, and ERROR_INVALID_ARGUMENT, ERROR_NOT_FOUND or ERROR_DRIVER_NOT_LOADED for the device at the probe's index, are `GPUAbsent` (`probe_nvml.go`). Any other error is a probe that failed, as before. A device lost after init shuts NVML down, so the next reading starts from `nvmlInit`.
2. **Each change of the state is one INFO line.** `GPU absent: <detail>` once when the card goes, `GPU present: <total> MB, <free> MB free` once when it comes back. Every reading passes through `Warden.read` (`absent.go`), the loop's and the fresh one at a start alike, so whichever reads the card first logs it.
3. **While the GPU is absent, its verdict resumes by the usual rule.** Nothing on the host can contend for a card the host does not have, and no model runs there. The reason reads `no GPU on this host: <detail>`, the probe error is empty, and `/warden/verdict` carries `gpu_absent` with the detail. `/warden/resources` answers 503 with the same reason. The Status tab shows the GPU as absent.
4. **A start of a model the one config places on the GPU is refused at once while the GPU is absent.** 503, `Retry-After: 30`, `X-InferMux-Absent: gpu`, error type `gpu_absent`. Failover then tries the model's next place, and logs that hop at debug, as it does a place at its limit (0019). The request gate refuses the request before llama-swap too, so a GPU place that started on the CPU before the absence was seen gets nothing more. Each model's refusal is logged once per absence.
5. **An absence older than one poll is read again at the next start**, through the fresh probe. A card handed back with `svm gpu host` is then used at the first start after it, whether or not the loop runs.
6. **A probe that fails for another reason warns once per detail**, and its recovery is one INFO line.

The agent's calls, made 2026-10-23 without the user:

- Refuse only models the one config names with the GPU among their devices. Outside the one config every model is on the GPU by default, and a host with no card has always run them on whatever llama-server found. Refusing there would stop a host that works today.
- Count ERROR_LIBRARY_NOT_FOUND as absent. On strix it is what NVML says before the generation has the driver's userspace, which on 2026-10-22 lasted until 23:37.
- 30 s for `Retry-After`. Only the user brings the card back, and 30 s is six polls, so a client that waits that long finds a card that has come.
- Read the card again only once the absence is a poll old, so a burst of starts does not open NVML once per start.
- Leave the verdict to resume by `resume_quiet_seconds` rather than at once. A card that vanished mid-yield and comes back gets the same quiet window it would have had.
- No `BusyHeader` on the refusal. Failover drops a busy 503 at the model's last place and answers "all destinations are busy", which would hide the reason from the client.

## Consequences

- An interactive request for a model whose only other places are batch-only, as `Octen-Embedding-4B.Q8_0` on strix, gets 503 `gpu_absent` while the zbox refuses it too, where it used to get a 2 to 9 s answer from the CPU. That is what the one config's comment asks for.
- The GPU's place no longer starts a CPU copy outside `idle.slice` next to the CPU place's own.
- Not handled: a card that disappears while a model runs on it. The request gate refuses that model's next request, but the running llama-server is not stopped.
- While NVML is initialised, the daemon holds `/dev/nvidia*` open, which `svm gpu guest` counts as a user of the card. Releasing NVML on request is not built.

## Verification

Tests, all with injected readings, since no test opens NVML (AGENTS.md):

- `internal/warden/absent_test.go`: 36 polls with the driver not loaded log one INFO line and no WARN, and the card's return logs one more; a yield ends while the card is absent; another failure warns once per detail and its recovery is logged; a start on the absent GPU is refused with the 503, the header and the body, a CPU place and an unplaced model start, and the GPU's start slot is left free; a card handed back is used at the next start after one poll, with the policy on and off; the start gate's own reading finds the absence with the loop not run; outside the one config nothing is refused; the request gate refuses before llama-swap and passes the CPU place.
- `internal/warden/probe_nvml_test.go`: which NVML return codes are absent, without calling NVML.
- `internal/failover/failover_test.go`, `TestAPlaceOnAnAbsentGPUIsLoggedAtDebug`: the hop logged at debug, and the 503 `gpu_absent` body with `Retry-After: 30` at the last place.
- `infermux_absent_test.go`: the daemon over a real llama-swap refuses a model on the absent GPU with the 503 and never runs its command, then starts it 1.1 s after the card appears, with `poll_seconds: 1`.

Ten mutations each failed at least one of them: the failover's debug line, the absent tick, the request gate's refusal, the re-read of a stale absence, the fit check's absence check, the once-per-detail guard, the classification of the return codes, the once-per-model refusal line, and the daemon's absence check at the start gate.

Live, on strix in the `rtx3080-passthrough` boot of 2026-10-23: the old build's behaviour in Context, read from the journal and `ps`. At 03:11 a scratch program, not committed, ran the new warden over strix's real NVML with no models and a fixed CPU reading: 12 ticks logged `GPU absent: nvml init: Driver Not Loaded` once and no WARN, the verdict read `no GPU on this host: nvml init: Driver Not Loaded` with no probe error, three starts of a model placed on the GPU were refused with one INFO line between them, and the CPU place's start passed. The daemon is not deployed, so the refusal through failover and the Status tab's absent card have not been seen on strix. The card's return was seen only with the old build, on 2026-10-22.
