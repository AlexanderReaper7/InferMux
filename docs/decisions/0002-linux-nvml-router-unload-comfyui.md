# 0002. Linux only: NVML, router unload, and ComfyUI as a second GPU tenant

- Date: 2026-09-28
- Status: accepted
- Rule: the warden runs on NixOS as a systemd service. It measures through NVML, yields by telling the llama.cpp router to unload its models, and treats ComfyUI's queue as contention and its idle time as a reason to send `/free`.
- Keeps 0001 whole: the warden decides and pushes, a pause is a message and not a lease, a failed probe leaves the verdict alone, `loading` counts as loaded.

## Context

The host moved from Windows 11 to NixOS on 2026-09-20. llama.cpp now runs as two systemd units from the NixOS configuration (`llama-cpp.service`, the router on :5001, and `llama-embed.service` on :5002, both in nixcfg's `modules/nixos/llm.nix`). nixcfg's decision log entry for 2026-09-25 listed "No GPU yielding yet" as an open item, so nothing yielded the card after the move.

The trigger was a second GPU workload on the same 10 GB: ComfyUI, added for image generation grounded in Blender renders (Z-Image-Turbo with the Fun ControlNet Union patch, and Qwen-Image-Edit as a GGUF). ComfyUI keeps its models on the GPU after a job and holds 6 to 12 GB until told otherwise. With a router model loaded, the two cannot both fit.

Most of the Windows warden had stopped applying. The PowerShell probe, the launcher, the preset editor, the text console and the tray icon all assumed the warden started llama-server itself.

## Decision

The user's choices, 2026-09-28:

1. **Linux only.** The Windows code is removed rather than kept behind a platform switch: `console.py`, `tools/`, `scripts/`, and `llama/` (the launcher and `models-preset.ini`). All of it survives in git history, last present at `b87f39b`. The preset's comments were the record of why each llama.cpp flag was set; nixcfg's `llm.nix` is where that record has to live now.
2. **Yield by unloading through the router's API**, not by stopping the unit. On the transition to *yielded* the warden sends `POST /models/unload {"model": id}` for each model the router holds. The router stays up. Only the transition: a client that asks for a model during the pause loads it again, and the warden does not fight that. Stopping `llama-cpp.service` was rejected because every client, LiteLLM included, would see connection refused for the whole pause.
3. **The warden frees ComfyUI.** Its queue (`GET /queue`, running plus pending) is read every tick. A job in it is contention, so the router is unloaded before ComfyUI has loaded much, rather than one tick after the utilization shows it. After `comfyui_idle_seconds` (600) of empty queue the warden sends `POST /free {"unload_models": true, "free_memory": true}`, once per idle stretch. Without that, the router could never resume: an idle ComfyUI's models keep free VRAM under the floor.

ComfyUI is deliberately not a *consumer* in 0001's sense. It has no announce endpoint and nothing to pause. It is a tenant the warden watches and cleans up after.

## Measurement

NVML through `nvidia-ml-py`, in-process. Measured on this host (RTX 3080, driver 615.71.09) on 2026-09-28, with Blender rendering through OptiX:

- A whole probe takes ~6 ms. The Windows probe took ~3.5 s, which is why `poll_seconds` drops from 30 to 5.
- `nvmlDeviceGetProcessUtilization` attributed 98% SM to the Blender process. On an idle card it raises `NVMLError_NotFound`, which is read as zero, not as a failed probe.
- **Per-process VRAM is attributable on Linux.** The compositor held 1672 MiB and Blender 2290 MiB. 0001's "VRAM cannot be attributed" was a WDDM fact.

*Ours* is decided by cgroup: a process whose `/proc/<pid>/cgroup` ends in one of `our_units` (`llama-cpp.service`, `llama-embed.service`). Names come from argv[0] with the nix wrapper's `.X-wrapped` taken off and Chromium's in-place argv rewrite cut at the first ` --`. Both files are world-readable, so the service runs as a `DynamicUser`.

## What was kept, and the open items

**The VRAM rule is unchanged.** Free VRAM is read only while the router holds no model. Linux could support a rule that also works while models are loaded, since the memory is attributable now. Nobody has decided to change it, so it stays as 0001 left it.

**`min_free_vram_mb` is 3000, not 6000.** The desktop alone holds 4.2 to 4.7 GB of the 10 GB (compositor, Firefox, Electron apps), so 6000 would read an idle desktop as contended and never resume. 3000 was picked to sit under that, not measured against what a model needs to load. The Windows comment said "~6 GB is what the smallest decode model needs". With `fit = on` the router offloads what does not fit, so a model loads in less at a throughput cost. Whether 3000 is the right floor is open.

**A race remains.** A ComfyUI job submitted between two ticks can start loading weights up to 5 s before the router is told to unload. ComfyUI's own memory manager then offloads or fails. The real fix is ComfyUI asking before it runs, which would be a lease or a hook in ComfyUI, and neither exists. Not built.

**A client can take VRAM back mid-pause** (point 2). Accepted by the user as the cost of keeping the router up.

## Cost

One NVML probe, one `GET /v1/models` and one `GET /queue` every 5 s, all on loopback.
