"""The measurement: who is using the GPU, how hard, and with how much memory.

NVML, the library under nvidia-smi, loaded in-process through `nvidia-ml-py`.
It replaced the Windows probe, a PowerShell sweep of the `GPU Engine` counters
that cost ~3.5 s a tick. Measured on the target box (RTX 3080, driver 615.71)
on 2026-09-28, while Blender rendered with OptiX:

* A whole probe takes ~6 ms.
* `nvmlDeviceGetProcessUtilization` attributed 98% SM to the Blender PID, the
  only busy one. On an idle card it raises `NVMLError_NotFound`, which means no
  process had a sample, not that the probe failed.
* Per-process memory IS attributable here: Blender 2290 MiB, the compositor
  1672 MiB. Under WDDM, `nvidia-smi --query-compute-apps` gave [N/A] for every
  process, and that is where 0001's "VRAM cannot be attributed" came from. It
  is reported per process now, but the policy still reads free VRAM only while
  our own models are unloaded (0002).

A process is *ours* when its cgroup is one of `Settings.our_units`, the systemd
units that run llama.cpp. ComfyUI is deliberately not ours: its work competes
with the router's for the same 10 GB, which is what makes it contention.

`/proc/<pid>/cmdline` and `/proc/<pid>/cgroup` are world-readable, so this runs
as an unprivileged user. `/proc/<pid>/exe` is not, which is why names come from
argv[0].
"""

from __future__ import annotations

import os
import threading
import time
from pathlib import Path

import pynvml

_lock = threading.Lock()
_initialised = False


def _init() -> None:
    global _initialised
    if not _initialised:
        pynvml.nvmlInit()
        _initialised = True


def process_name(pid: int, proc: Path = Path("/proc")) -> str:
    """argv[0]'s basename, with the nix wrapper's disguise taken off.

    `makeWrapper` renames the real binary `.blender-wrapped`, and `comm` cuts
    that to `.blender-wrappe`, so neither reads as the program's name.

    Chromium and Electron overwrite their argv with one space-joined string,
    "vesktop --gpu-preferences=...", so the name ends at the first " --" (seen
    live 2026-09-28). Splitting on the first space instead would cut
    "T3 Code (Alpha)" to "T3"."""
    try:
        argv0 = (proc / str(pid) / "cmdline").read_bytes().split(b"\0", 1)[0].decode()
    except OSError:
        argv0 = ""
    if not argv0:
        try:
            argv0 = (proc / str(pid) / "comm").read_text().strip()
        except OSError:
            return "unknown"
    name = os.path.basename(argv0.split(" --", 1)[0])
    if name.startswith("."):
        name = name[1:]
    for suffix in ("-wrapped", "-wrappe"):
        name = name.removesuffix(suffix)
    return name or "unknown"


def process_unit(pid: int, proc: Path = Path("/proc")) -> str | None:
    """The last component of the process's cgroup path: a `.service` or a
    `.scope`. None when the process is gone or the file is unreadable."""
    try:
        line = (proc / str(pid) / "cgroup").read_text().splitlines()[0]
    except (OSError, IndexError):
        return None
    return line.rsplit("/", 1)[-1] or None


class NvmlProbe:
    """One device's processes, sampled since the previous call.

    Stateful only in `_last_seen`: NVML returns the utilization samples newer
    than the timestamp it is given, so each tick asks for what happened since
    the last one."""

    def __init__(
        self, our_units: tuple[str, ...], *, device: int = 0, proc: Path = Path("/proc")
    ) -> None:
        self.our_units = frozenset(our_units)
        self.device = device
        self.proc = proc
        self._last_seen = 0

    def __call__(self) -> dict:
        with _lock:
            _init()
            handle = pynvml.nvmlDeviceGetHandleByIndex(self.device)
            memory = pynvml.nvmlDeviceGetMemoryInfo(handle)
            rates = pynvml.nvmlDeviceGetUtilizationRates(handle)

            # A process in both lists (C+G) reports the same figure in each.
            vram: dict[int, int] = {}
            for listing in (
                pynvml.nvmlDeviceGetComputeRunningProcesses,
                pynvml.nvmlDeviceGetGraphicsRunningProcesses,
            ):
                for row in listing(handle):
                    mb = (row.usedGpuMemory or 0) // 2**20
                    vram[row.pid] = max(vram.get(row.pid, 0), mb)

            since = self._last_seen or int((time.time() - 1.0) * 1e6)
            self._last_seen = int(time.time() * 1e6)
            util: dict[int, float] = {}
            try:
                for sample in pynvml.nvmlDeviceGetProcessUtilization(handle, since):
                    util[sample.pid] = max(util.get(sample.pid, 0.0), float(sample.smUtil))
            except pynvml.NVMLError_NotFound:
                pass  # no process had a sample in the window: an idle card

        return summarise(
            vram=vram,
            util=util,
            used_mb=memory.used // 2**20,
            total_mb=memory.total // 2**20,
            gpu_percent=rates.gpu,
            name=lambda pid: process_name(pid, self.proc),
            unit=lambda pid: process_unit(pid, self.proc),
            our_units=self.our_units,
        )


def summarise(
    *,
    vram: dict[int, int],
    util: dict[int, float],
    used_mb: int,
    total_mb: int,
    gpu_percent: float | None,
    name,
    unit,
    our_units: frozenset[str],
) -> dict:
    """The raw readings as the policy reads them. Separate from the NVML calls
    so the attribution is testable without a GPU."""
    processes = []
    for pid in sorted(set(vram) | set(util)):
        where = unit(pid)
        processes.append(
            {
                "pid": pid,
                "name": name(pid),
                "unit": where,
                "percent": util.get(pid, 0.0),
                "vram_mb": vram.get(pid, 0),
                "ours": where in our_units,
            }
        )
    foreign = sum(p["percent"] for p in processes if not p["ours"])
    ours = sum(p["percent"] for p in processes if p["ours"])
    busiest = sorted(processes, key=lambda p: (-p["percent"], -p["vram_mb"]))
    return {
        "foreign_gpu_percent": round(foreign, 1),
        "our_gpu_percent": round(ours, 1),
        "our_vram_mb": sum(p["vram_mb"] for p in processes if p["ours"]),
        "vram_used_mb": used_mb,
        "vram_total_mb": total_mb,
        "vram_free_mb": total_mb - used_mb,
        "gpu_percent": gpu_percent,
        # Who to name in a verdict: the foreign processes doing work.
        "culprits": [p["name"] for p in busiest if not p["ours"] and p["percent"] > 0][:3],
        # Capped: the reader wants "who is using the GPU", not a census of
        # every compositor client on the box.
        "processes": busiest[:10],
        "sampled_at": time.time(),
    }
