# /// script
# requires-python = ">=3.12"
# dependencies = ["fastapi", "uvicorn"]
# ///
"""Host-side control agent for llama.cpp — the one process that crosses the
Docker/host boundary.

Episteme runs in Docker; llama.cpp runs natively on Windows for GPU access. A
container cannot start a host process, cannot signal one, and cannot read
another process's console output. This agent is the crossing: a small loopback
HTTP service on the host that Episteme calls.

It merges two things the design docs specified separately — the llama.cpp
lifecycle controller (handoff-llama-control.md §4b) and the "idle monitor"
(architecture §7 *Scheduling & idle behavior*). They want the same privileges on
the same box, so they are one process, not two.

Deliberately a *sensor and actuator*, never a decision-maker: `/resources`
reports measurements and Episteme's governor owns the policy. That keeps every
threshold in Episteme's config next to the rest of the tuning, keeps this file
stateless, and means the agent being down degrades to "no opinion" — which is
exactly today's behaviour — rather than to a stale flag nobody clears.

Run (from the repo root, on the host):

    uv run hostagent/llama_agent.py

Binds 127.0.0.1 only. That is both safe and sufficient: Docker Desktop proxies
`host.docker.internal` from the host side, which is why the containers already
reach llama-server's own loopback-bound :5001.
"""

import argparse
import json
import logging
import re
import socket
import subprocess
import threading
import time
from pathlib import Path

import uvicorn
from fastapi import FastAPI, HTTPException, Query

log = logging.getLogger("llama-agent")

LLAMA_DIR = Path(r"C:\selfhosting\llama-cpp")
LAUNCHER = LLAMA_DIR / "launch-llama-v2.ps1"
LOG_DIR = LLAMA_DIR / "logs"
# Server names must match the launcher's -LogFile naming and its port constants.
SERVERS = {"router": 5001, "embed": 5002}
PROCESS_NAME = "llama-server"

# llama-server colors its output, and `--log-file` gets the escape codes verbatim
# — a log pane would render "\x1b[34m0.00.193\x1b[0m" as literal noise. Stripped
# here rather than by passing `--log-colors off`, so the file reads correctly no
# matter how the server was started, and a human running the launcher in a
# console keeps their colors there.
ANSI_RE = re.compile(r"\x1b\[[0-9;]*[A-Za-z]")

# One PowerShell round-trip per probe instead of one per fact: pwsh costs ~400ms
# to start, and both sensors need it (nvidia-smi is a subprocess either way, and
# per-process GPU utilization is only available through a perf counter).
#
# Budget, measured on the target box: Get-Counter over all 621 GPU-engine
# instances is 3.3s and dominates everything else (nvidia-smi 109ms, Get-Process
# 30ms). ~1s of that is PDH's own sampling floor — rate counters need two samples
# — so it is irreducible without giving up per-process attribution, which is the
# whole reason this sensor is trustworthy. The cost is paid off-page instead: the
# admin panel loads /resources through an htmx fragment, and the governor polls
# it on a cron. Nothing waits on it synchronously.
#
# Why these two sensors and not others — all three alternatives were measured on
# the target box (RTX 3080 10GB) while a game was running:
#   * `nvidia-smi --query-compute-apps` gives per-process memory as [N/A] under
#     WDDM. Useless for attribution.
#   * The `GPU Process Memory` counter over-reports badly (dwm alone claimed
#     22 GB on a 10 GB card — it counts committed, not resident).
#   * `GPU Engine ... Utilization Percentage` IS per-process and truthful: it
#     showed the game's PID at 92.4% as the only busy instance.
# So: utilization is attributable, memory is not. Utilization is therefore
# reported per-process (letting the caller exclude our own load), and memory only
# in total — honest about what each measurement can carry.
PROBE_PS = r"""
$ErrorActionPreference = 'SilentlyContinue'

$llama = @(Get-Process -Name 'llama-server' | Select-Object -ExpandProperty Id)

# Per-process GPU utilization. Instance names look like
# "pid_21544_luid_..._eng_0_engtype_3d"; sum every engine per PID, since a
# process can be busy on 3D, compute and copy engines at once.
$byPid = @{}
foreach ($s in (Get-Counter '\GPU Engine(*)\Utilization Percentage').CounterSamples) {
    if ($s.CookedValue -le 0) { continue }
    if ($s.InstanceName -notmatch '^pid_(\d+)_') { continue }
    $p = [int]$Matches[1]
    $byPid[$p] = [double]$byPid[$p] + $s.CookedValue
}

$procs = @()
foreach ($p in $byPid.Keys) {
    $proc = Get-Process -Id $p
    $procs += [pscustomobject]@{
        pid     = $p
        name    = $(if ($proc) { $proc.ProcessName } else { 'unknown' })
        percent = [math]::Round($byPid[$p], 1)
        ours    = $llama -contains $p
    }
}

# Total VRAM only — see the note above on why this is not per-process.
$vram = @{ used_mb = $null; total_mb = $null; utilization = $null }
$smi = & nvidia-smi --query-gpu=utilization.gpu,memory.used,memory.total --format=csv,noheader,nounits 2>$null
if ($LASTEXITCODE -eq 0 -and $smi) {
    $f = ($smi | Select-Object -First 1) -split ',\s*'
    $vram = @{ utilization = [int]$f[0]; used_mb = [int]$f[1]; total_mb = [int]$f[2] }
}

@{
    processes = @($procs)
    vram      = $vram
    running   = @(Get-Process | Select-Object -ExpandProperty ProcessName -Unique)
} | ConvertTo-Json -Depth 4 -Compress
"""

# Windows' own catalogue of installed games — Game Bar populates it, so it is a
# watchlist nobody has to maintain by hand (310 entries on the target box). Read
# separately and cached: it changes when you install a game, not when you play
# one, and walking those registry keys costs 250ms every probe otherwise.
#
# It reports that a game is *running*, which is not the same claim as "the GPU is
# contended" — so it is context for the admin panel, not something the governor
# decides on. Utilization is the signal; this explains it.
GAMES_PS = r"""
$ErrorActionPreference = 'SilentlyContinue'
$names = foreach ($c in (Get-ChildItem 'HKCU:\System\GameConfigStore\Children')) {
    $exe = (Get-ItemProperty $c.PSPath).MatchedExeFullPath
    if ($exe) { [System.IO.Path]::GetFileNameWithoutExtension($exe) }
}
@($names | Sort-Object -Unique) | ConvertTo-Json -Compress
"""
GAMES_CACHE_SECONDS = 600.0
_games_cache: tuple[float, set[str]] = (0.0, set())


def _known_games() -> set[str]:
    global _games_cache
    cached_at, names = _games_cache
    if time.monotonic() - cached_at < GAMES_CACHE_SECONDS and names:
        return names
    try:
        parsed = json.loads(_run_ps(GAMES_PS, timeout=30.0).stdout or "[]")
    except (subprocess.TimeoutExpired, json.JSONDecodeError, OSError):
        return names  # keep whatever we had; this is context, not a control input
    # ConvertTo-Json collapses a one-element array to a scalar.
    names = {parsed} if isinstance(parsed, str) else set(parsed)
    _games_cache = (time.monotonic(), names)
    return names


def _run_ps(script: str, timeout: float = 30.0) -> subprocess.CompletedProcess:
    return subprocess.run(
        ["powershell", "-NoProfile", "-NonInteractive", "-Command", script],
        capture_output=True,
        text=True,
        timeout=timeout,
    )


# Process identity for the panel (which PID, up since when). Matched to a server
# by the `--port N` in its own command line rather than by asking the OS who owns
# the socket: `Get-NetTCPConnection` costs 3.1s in a fresh process (it imports
# NetTCPIP every time), against 885ms for this and ~1ms for the socket probe that
# already answers the question that matters.
PROCESS_PS = r"""
$ErrorActionPreference = 'SilentlyContinue'
@(Get-CimInstance Win32_Process -Filter "Name='llama-server.exe'" | ForEach-Object {
    [pscustomobject]@{
        pid        = $_.ProcessId
        cmdline    = $_.CommandLine
        started_at = $_.CreationDate.ToString('o')
    }
}) | ConvertTo-Json -Depth 3 -Compress
"""


def _port_open(port: int, timeout: float = 0.5) -> bool:
    """Liveness by connecting, not by inspecting the OS socket table. It is three
    orders of magnitude cheaper, and it tests the thing Episteme actually depends
    on — that something accepts a connection on that port."""
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=timeout):
            return True
    except OSError:
        return False


def _server_status() -> dict[str, dict]:
    """Per-server liveness, plus PID/uptime when there is a process to describe.

    The subprocess is skipped entirely when nothing is listening — which is
    exactly the case where someone is staring at the admin page wondering why the
    backend is down, so that path costs ~1ms and no PowerShell at all."""
    live = {name: _port_open(port) for name, port in SERVERS.items()}
    processes: list[dict] = []
    if any(live.values()):
        try:
            parsed = json.loads(_run_ps(PROCESS_PS, timeout=30.0).stdout or "[]")
            processes = [parsed] if isinstance(parsed, dict) else parsed
        except (subprocess.TimeoutExpired, json.JSONDecodeError, OSError):
            processes = []

    rows = {}
    for name, port in SERVERS.items():
        match = next(
            (p for p in processes if f"--port {port}" in (p.get("cmdline") or "")), None
        )
        rows[name] = {
            "port": port,
            "listening": live[name],
            "pid": (match or {}).get("pid"),
            "started_at": (match or {}).get("started_at"),
            "log": str(LOG_DIR / f"{name}.log"),
        }
    return rows


app = FastAPI(title="llama.cpp host agent")


@app.get("/status")
def status() -> dict:
    return {"servers": _server_status(), "launcher": str(LAUNCHER)}


@app.get("/resources")
def resources() -> dict:
    """Raw measurements; the caller decides what counts as busy.

    `foreign_gpu_percent` is the one number a policy actually wants: total GPU
    utilization minus our own llama-server processes. It is valid whether or not
    we are generating, which is what makes it usable as a *pause* signal and not
    only as a start gate — unlike VRAM, which cannot be attributed at all."""
    try:
        result = _run_ps(PROBE_PS)
        data = json.loads(result.stdout or "{}")
    except (subprocess.TimeoutExpired, json.JSONDecodeError, OSError) as exc:
        raise HTTPException(status_code=503, detail=f"probe failed: {exc}") from exc

    processes = data.get("processes") or []
    vram = data.get("vram") or {}
    games = sorted(_known_games().intersection(data.get("running") or []))
    foreign = sum(p["percent"] for p in processes if not p.get("ours"))
    ours = sum(p["percent"] for p in processes if p.get("ours"))
    return {
        "foreign_gpu_percent": round(foreign, 1),
        "our_gpu_percent": round(ours, 1),
        "vram_used_mb": vram.get("used_mb"),
        "vram_total_mb": vram.get("total_mb"),
        "vram_free_mb": (
            None
            if vram.get("total_mb") is None
            else vram["total_mb"] - (vram.get("used_mb") or 0)
        ),
        "gpu_percent": vram.get("utilization"),
        "games_running": games,
        # Sorted busiest-first and capped: the admin panel wants "who is using
        # the GPU", not a census of every window compositor on the box.
        "processes": sorted(processes, key=lambda p: -p["percent"])[:10],
        "sampled_at": time.time(),
    }


# Serializes /start. A "is it already listening?" check on its own is a TOCTOU
# race and loses it: two clicks that arrive before the first server has bound
# both see nothing listening and both launch. Measured, not theorized — two
# requests 300ms apart produced FOUR llama-server processes. The launcher's own
# `Get-NetTCPConnection` guard has exactly the same hole, so it cannot be relied
# on either.
#
# The lock is held through the port wait, not just the check, so the second
# caller blocks until the first has actually finished and then observes the true
# state. It waits rather than being rejected, because "already running" is the
# honest answer to give it.
#
# Every lifecycle route takes it, not just /start: a stop landing between another
# caller's check and its launch is the same race with a worse outcome. Reentrant
# so /restart can hold it across the whole stop-then-start, which is the point of
# restart being one operation rather than two requests.
_lifecycle_lock = threading.RLock()


@app.post("/start")
def start() -> dict:
    with _lifecycle_lock:
        before = _server_status()
        if all(row["listening"] for row in before.values()):
            return {"started": False, "reason": "already running", "servers": before}

        result = _run_ps(f'& "{LAUNCHER}" server -Detached', timeout=120.0)
        output = ((result.stdout or "") + (result.stderr or "")).strip()
        if result.returncode != 0:
            raise HTTPException(
                status_code=500, detail=f"launcher exited {result.returncode}: {output[-500:]}"
            )

        # A zero exit code is NOT evidence that the servers came up. PowerShell's
        # default $ErrorActionPreference is Continue, so a launcher that failed on
        # a missing model, a bad preset or an already-bound port writes its
        # complaint to the console and still exits 0 — and reporting that as
        # `started: true` sends the operator to a working-looking panel with
        # nothing behind it, while the one thing that explains it (the captured
        # output) is thrown away. The ports are the actual claim, so verify them.
        after = _wait_for_ports(listening=True)
        down = [name for name, row in after.items() if not row["listening"]]
        if down:
            raise HTTPException(
                status_code=500,
                detail=(
                    f"launcher ran but {', '.join(down)} never came up: "
                    f"{output[-500:] or 'no launcher output'}"
                ),
            )
        return {"started": True, "servers": after, "output": output[-2000:]}


@app.post("/stop")
def stop() -> dict:
    """Kills both llama-servers. Episteme is expected to have paused the pipeline
    and waited for a work-unit boundary first — this endpoint has no idea whether
    a generation is in flight, and says so rather than pretending to check.

    Kills by process name, so it also cleans up any orphan a previous crash or
    race left behind."""
    with _lifecycle_lock:
        _run_ps(
            f"Get-Process -Name '{PROCESS_NAME}' -ErrorAction SilentlyContinue | Stop-Process -Force"
        )
        return {"stopped": True, "servers": _wait_for_ports(listening=False)}


@app.post("/restart")
def restart() -> dict:
    with _lifecycle_lock:
        stop()
        return start()


def _wait_for_ports(*, listening: bool, timeout: float = 60.0) -> dict[str, dict]:
    """Poll until every server reaches the wanted state, or `timeout`. Returning
    a half-transitioned status is worse than waiting: the caller renders it.

    Polls with the cheap socket probe and pays for the full status — which shells
    out for PID and uptime — exactly once, at the end."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if all(_port_open(port) is listening for port in SERVERS.values()):
            break
        time.sleep(1.0)
    return _server_status()


@app.get("/logs")
def logs(
    which: str = Query("router"),
    tail: int = Query(200, ge=1, le=5000),
    since: int | None = Query(None, ge=0),
) -> dict:
    """HTTP face of `read_log`. Kept a one-liner because a route's defaults are
    `Query` objects, not values: anything calling it in-process (the tests do)
    would get a `Query` wherever it omitted an argument, which is harmless right
    up until that argument starts being used in arithmetic."""
    return read_log(which, tail, since)


def read_log(which: str, tail: int = 200, since: int | None = None) -> dict:
    """Two modes over the same file, both a bounded read:

    * `since` omitted — the last `tail` lines. Seeks from the end rather than
      reading the file, so a log that grew to gigabytes overnight still costs a
      fixed read.
    * `since=N` — only the bytes written after offset N. This is what a live
      pane needs: it APPENDS the answer instead of replacing its contents with
      a freshly re-fetched tail, which is both ~99% less data and the only way
      the reader's text selection and scrollback survive an update.

    Always returns `next_offset` to pass back on the next call. A trailing
    partial line — the server is writing while we read — is withheld rather
    than shipped truncated, and `next_offset` stops before it, so it arrives
    complete on the following call.

    `reset: true` means the caller's offset is meaningless and it must REPLACE
    what it is showing rather than append: either the file shrank under it (the
    launcher truncates the log on every start, so an offset from before a
    restart points into the middle of a different file) or the backlog exceeded
    the tail window, in which case `gap_bytes` says how much was skipped so the
    pane can admit the discontinuity instead of quietly splicing.
    """
    if which not in SERVERS:
        raise HTTPException(status_code=404, detail=f"unknown log {which!r}")
    path = LOG_DIR / f"{which}.log"
    if not path.exists():
        # `reset` matters here: a caller holding an offset from before a restart
        # must drop what it has, not wait to append onto it.
        return {
            "log": which,
            "path": str(path),
            "exists": False,
            "lines": [],
            "next_offset": 0,
            "reset": bool(since),
        }

    # 400 bytes/line is generous for llama-server output; if the tail-end chunk
    # turns out to hold fewer lines than asked for, that is the whole file. The
    # same figure bounds the delta read, so neither mode can slurp the file.
    window = tail * 400
    with path.open("rb") as handle:
        handle.seek(0, 2)
        size = handle.tell()
        # Shrunk under the caller, or further behind than the window: either way
        # its offset cannot be appended onto, so fall back to a tail read.
        reset = since is not None and (since > size or size - since > window)
        from_end = since is None or reset
        gap_bytes = max(0, size - window - since) if reset else 0
        start = size - min(size, window) if from_end else since
        handle.seek(start)
        chunk = handle.read(size - start)

    # Cut at the last newline: whatever follows it is a line the server is still
    # writing, and `next_offset` must stop short of it so it arrives whole next
    # time. A seek from the end also lands mid-line at the head, which is the
    # one place a truncated line would otherwise be rendered.
    end = chunk.rfind(b"\n")
    text = chunk[: end + 1].decode("utf-8", errors="replace") if end >= 0 else ""
    if from_end and start > 0:
        text = text.split("\n", 1)[-1] if "\n" in text else ""
    if end >= 0:
        next_offset = start + end + 1
    else:
        # Nothing complete in the window. Realigning to a mid-line offset would
        # emit a truncated line later, so a tail read gives up on it entirely
        # while a delta read simply waits where it is.
        next_offset = size if from_end else start
    return {
        "log": which,
        "path": str(path),
        "exists": True,
        "size_bytes": size,
        "lines": [ANSI_RE.sub("", line) for line in text.splitlines()[-tail:]],
        "next_offset": next_offset,
        "reset": reset,
        "gap_bytes": gap_bytes,
    }


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--port", type=int, default=5003)
    parser.add_argument("--host", default="127.0.0.1")
    args = parser.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    LOG_DIR.mkdir(parents=True, exist_ok=True)
    uvicorn.run(app, host=args.host, port=args.port, log_level="info")


if __name__ == "__main__":
    main()
