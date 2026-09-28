# CLAUDE-TODO.md

Claude's working list for llama-warden: what is built and shipping but has not been *watched running*. The point is to stop a claim that something works when all there is is code that exists and tests that pass. Move an item out when it has been observed, and say in the commit what was observed.

## Watched so far on Linux (2026-09-28)

- The probe against the real card: processes named and attributed to their cgroup units, free VRAM read. Chromium's argv rewrite was found this way and fixed.
- The built package run with thresholds it could not cross: `/verdict`, `/status` and `/resources` answered, and `/status` listed the router's four models.

## Not yet verified live

- **A yield on the real host.** Foreign utilization or a ComfyUI job crossing the threshold, the router answering `POST /models/unload`, and its models reading `unloaded` afterwards.
- **The service under its systemd sandbox.** `DynamicUser` reading other users' `/proc/<pid>/cgroup`, NVML seeing the processes of `llama-cpp.service` (itself a `DynamicUser`) from a different uid.
- **ComfyUI's `/free` after the idle window**, and the VRAM actually coming back.
- **The warden deciding to RESUME.** Carried over from Windows: the quiet window elapsing and `resume` going out on its own has never been seen, there or here.
- **The race in 0002**: what ComfyUI does when a job starts loading into a card the router has not yet let go of.
