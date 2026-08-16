# /// script
# requires-python = ">=3.12"
# dependencies = ["fastapi", "uvicorn", "textual", "pystray", "pillow"]
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

    uv run hostagent/llama_agent.py             # text UI + tray icon, if a console exists
    uv run hostagent/llama_agent.py --headless  # the bare HTTP service, as it always was

Binds 127.0.0.1 only. That is both safe and sufficient: Docker Desktop proxies
`host.docker.internal` from the host side, which is why the containers already
reach llama-server's own loopback-bound :5001.

The UI lives in `console.py` and is strictly a face: every decision, threshold
and side effect is still here, so `--headless` is not a reduced agent, it is the
same agent with nobody watching.
"""

import argparse
import json
import logging
import logging.handlers
import re
import shutil
import socket
import subprocess
import sys
import threading
import time
from collections import deque
from datetime import datetime
from pathlib import Path

import uvicorn
from fastapi import Body, FastAPI, HTTPException, Query

log = logging.getLogger("llama-agent")

# The agent's own log lines, for the console's third tab. A bounded deque rather
# than a file tail because these lines have not been written anywhere yet at the
# moment the UI wants them, and re-reading our own file to display what we just
# logged would be a round trip through the disk for no reason.
RECORDS: deque[str] = deque(maxlen=2000)

LLAMA_DIR = Path(r"C:\selfhosting\llama-cpp")
LAUNCHER = LLAMA_DIR / "launch-llama-v2.ps1"
LOG_DIR = LLAMA_DIR / "logs"
PRESET = LLAMA_DIR / "models-preset.ini"
PRESET_BACKUP_PREFIX = "models-preset.ini.bak-"
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


def start_servers(extra_args: list[str] | None = None) -> dict:
    """Bring both servers up. Idempotent.

    `extra_args` is appended to the launcher's own argument list, which passes it
    through to llama-server (`-PassthroughArgs`). It exists for benchmark sweeps,
    which need the same server brought up with one flag changed.

    Still no policy here (0023, 0041): the agent does not decide what a good
    configuration is, it applies the one it was handed and reports what came
    back. Every argument's meaning lives in Episteme, next to the run row that
    recorded why it was tried.

    Split from the route below because `restart` calls it, and because a FastAPI
    handler is a poor plain function: its parameter default is a `Body` marker
    object, not a value, so calling it directly hands the body-reading code a
    sentinel."""
    extra_args = list(extra_args or [])
    with _lifecycle_lock:
        before = _server_status()
        if all(row["listening"] for row in before.values()):
            return {"started": False, "reason": "already running", "servers": before}

        command = f'& "{LAUNCHER}" server -Detached'
        if extra_args:
            command += " " + " ".join(f"'{arg}'" for arg in extra_args)
        result = _run_ps(command, timeout=120.0)
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


@app.post("/start")
def start(body: dict | None = Body(default=None)) -> dict:
    return start_servers(_safe_args((body or {}).get("extra_args") or []))


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
def restart(body: dict | None = Body(default=None)) -> dict:
    extra_args = _safe_args((body or {}).get("extra_args") or [])
    with _lifecycle_lock:
        stop()
        return start_servers(extra_args)


# --- Model configuration surface (0041) --------------------------------------------
#
# The preset file is edited LINE BY LINE rather than round-tripped through
# configparser, and that is not fussiness. models-preset.ini is half comments,
# and those comments are the only record of why a setting is what it is - the
# `ngl = 999` removal that tripled throughput on 2026-08-15 is eleven lines of
# explanation above a deleted key. A writer that dropped them would destroy more
# knowledge in one call than the whole benchmarking feature produces.

_ASSIGNMENT = re.compile(r"^(\s*)([A-Za-z0-9_.\-]+)(\s*=\s*)(.*?)(\s*)$")
_SECTION = re.compile(r"^\s*\[(.+?)\]\s*$")


def _parse_preset(text: str) -> dict[str, dict[str, str]]:
    """Sections to key/value, comments discarded. For READING only: nothing
    writes back from this, so losing the comments here is harmless."""
    sections: dict[str, dict[str, str]] = {}
    current = ""
    for line in text.splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith((";", "#")):
            continue
        if match := _SECTION.match(line):
            current = match.group(1)
            sections.setdefault(current, {})
            continue
        if match := _ASSIGNMENT.match(line):
            sections.setdefault(current, {})[match.group(2)] = match.group(4)
    return sections


def _edit_preset(text: str, overrides: dict[str, dict]) -> tuple[str, list[str]]:
    """Apply `{section: {key: value_or_None}}`; None deletes the key.

    A key not present in its section is appended at the section's end (before any
    trailing blank lines, so the file does not grow a gap per edit). A section
    that does not exist is created at the end of the file. Returns the new text
    and a human-readable list of what changed, which is what the caller stores
    beside the numbers.
    """
    lines = text.splitlines()
    changed: list[str] = []
    # Section name -> [start_index, end_index) over `lines`.
    bounds: dict[str, list[int]] = {}
    current = ""
    start = 0
    for index, line in enumerate(lines):
        if match := _SECTION.match(line):
            bounds.setdefault(current, [start, index])[1] = index
            current = match.group(1)
            start = index + 1
            bounds[current] = [start, len(lines)]
    bounds.setdefault(current, [start, len(lines)])[1] = len(lines)

    # Applied bottom-up so an insertion never invalidates a later section's
    # recorded bounds.
    for section in sorted(overrides, key=lambda name: -bounds.get(name, [len(lines)])[0]):
        wanted = overrides[section]
        if section not in bounds:
            lines.extend(["", f"[{section}]"])
            bounds[section] = [len(lines), len(lines)]
        begin, end = bounds[section]
        for key, value in wanted.items():
            hit = None
            for index in range(begin, min(end, len(lines))):
                match = _ASSIGNMENT.match(lines[index])
                if match and match.group(2) == key:
                    hit = index
                    break
            if value is None:
                if hit is not None:
                    changed.append(f"[{section}] removed {key} (was {lines[hit].strip()})")
                    # Commented out rather than deleted: this file's own history
                    # is written in its comments, and a sweep that silently
                    # vanished a line would be the exact failure mode the
                    # line-based writer exists to avoid.
                    lines[hit] = f"; [benchmark] {lines[hit].strip()}"
                continue
            if hit is not None:
                match = _ASSIGNMENT.match(lines[hit])
                if match.group(4) == str(value):
                    continue
                changed.append(f"[{section}] {key}: {match.group(4)} -> {value}")
                lines[hit] = f"{match.group(1)}{key}{match.group(3)}{value}"
            else:
                tail = end
                while tail > begin and not lines[tail - 1].strip():
                    tail -= 1
                lines.insert(tail, f"{key} = {value}")
                changed.append(f"[{section}] {key} = {value} (added)")
    return "\n".join(lines) + "\n", changed


def _backup_path(name: str) -> Path:
    """Resolve a caller-supplied backup name, refusing anything that is not one
    of ours in the llama.cpp directory. The agent binds loopback only, but a path
    parameter that writes over arbitrary files is not something to leave resting
    on the network boundary."""
    candidate = (LLAMA_DIR / Path(name).name).resolve()
    if candidate.parent != LLAMA_DIR.resolve() or not candidate.name.startswith(
        PRESET_BACKUP_PREFIX
    ):
        raise HTTPException(status_code=400, detail=f"not a preset backup: {name}")
    if not candidate.exists():
        raise HTTPException(status_code=404, detail=f"no such backup: {candidate}")
    return candidate


@app.get("/preset")
def preset() -> dict:
    if not PRESET.exists():
        raise HTTPException(status_code=404, detail=f"no preset at {PRESET}")
    text = PRESET.read_text(encoding="utf-8")
    return {"path": str(PRESET), "text": text, "sections": _parse_preset(text)}


@app.post("/preset")
def preset_apply(body: dict = Body(...)) -> dict:
    """Apply overrides, after copying the current file aside.

    The backup is returned rather than remembered, because the agent is
    stateless by design (0023) and the caller is the one that knows when the
    experiment is over. A sweep threads the FIRST backup through every variant so
    the restore at the end puts back the operator's real preset, not variant
    three's edit of variant two's."""
    sections = body.get("sections") or {}
    if not isinstance(sections, dict) or not sections:
        raise HTTPException(status_code=422, detail="sections must be a non-empty object")
    if not PRESET.exists():
        raise HTTPException(status_code=404, detail=f"no preset at {PRESET}")
    with _lifecycle_lock:
        stamp = datetime.now().strftime("%Y%m%d-%H%M%S")
        backup = LLAMA_DIR / f"{PRESET_BACKUP_PREFIX}{stamp}"
        shutil.copy2(PRESET, backup)
        text, changed = _edit_preset(PRESET.read_text(encoding="utf-8"), sections)
        PRESET.write_text(text, encoding="utf-8")
    log.info("Preset edited (%d changes), backup at %s", len(changed), backup)
    return {"backup": backup.name, "changed": changed, "path": str(PRESET)}


@app.post("/preset/restore")
def preset_restore(body: dict = Body(...)) -> dict:
    backup = _backup_path(body.get("backup") or "")
    with _lifecycle_lock:
        shutil.copy2(backup, PRESET)
    log.info("Preset restored from %s", backup)
    return {"restored": True, "from": backup.name, "path": str(PRESET)}


def _safe_args(args: list) -> list[str]:
    """Passthrough arguments are interpolated into a PowerShell command line, so
    they are constrained to what a llama-server flag or value can look like. Not
    a trust boundary against Episteme (which is the only caller and can already
    start processes here), but a command line assembled by string joining should
    not be one quote away from arbitrary execution."""
    safe = re.compile(r"^[A-Za-z0-9_.:\\/=,+-]+$")
    for arg in args:
        if not isinstance(arg, str) or not safe.match(arg):
            raise HTTPException(status_code=422, detail=f"unsafe launcher argument: {arg!r}")
    return list(args)


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
    # Every line on disk ends "\r\r\n": llama-server writes CRLF and the Windows
    # CRT translates the \n a second time. splitlines() reads the orphan \r as a
    # break of its own, which is a blank line between every real one, in the
    # admin pane and the agent console alike. A LONE \r stays a break on purpose:
    # llama.cpp uses it to rewrite progress in place, and those are separate
    # lines that a log file cannot render any other way.
    text = text.replace("\r\r\n", "\n").replace("\r\n", "\n")
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


class _DequeHandler(logging.Handler):
    """Feeds `RECORDS`, which is the console's `agent` tab."""

    def emit(self, record: logging.LogRecord) -> None:
        RECORDS.append(self.format(record))


def _configure_logging(*, to_console: bool) -> None:
    """A rotating file, the deque, and stdout only when nothing is drawing on it.

    A `StreamHandler` under the text UI would scribble log lines over Textual's
    own output and corrupt the display, and uvicorn's default config installs
    exactly that. Hence `log_config=None` at the uvicorn call: root owns the
    handlers, and there is one policy rather than two.

    The file rotates rather than truncating, unlike the llama-server logs the
    launcher manages. Those are megabytes an hour and their value is entirely in
    the present; this one is a few kilobytes a day and its value is mostly in the
    run that crashed, which truncate-on-start is precisely how to lose.
    """
    LOG_DIR.mkdir(parents=True, exist_ok=True)
    formatter = logging.Formatter("%(asctime)s %(levelname)s %(message)s")
    handlers: list[logging.Handler] = [
        logging.handlers.RotatingFileHandler(
            LOG_DIR / "agent.log", maxBytes=2_000_000, backupCount=1, encoding="utf-8"
        ),
        _DequeHandler(),
    ]
    if to_console:
        handlers.append(logging.StreamHandler())
    root = logging.getLogger()
    root.setLevel(logging.INFO)
    for handler in handlers:
        handler.setFormatter(formatter)
        root.addHandler(handler)


def _console_api():
    """Bind the console's `AgentAPI` to this module's functions.

    Imported here, not at module scope, for two reasons: `--headless` should not
    need textual or pystray installed to run, and `tests/test_hostagent.py` loads
    this file by path, where a sibling import would not resolve.
    """
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    import console

    return console, console.AgentAPI(
        server_status=_server_status,
        resources=resources,
        read_log=read_log,
        start=start_servers,
        stop=stop,
        restart=lambda: restart(None),
        servers=SERVERS,
        log_dir=LOG_DIR,
        records=RECORDS,
    )


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--port", type=int, default=5003)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument(
        "--headless", action="store_true", help="HTTP service only: no text UI, no tray icon"
    )
    parser.add_argument(
        "--hide",
        action="store_true",
        help=(
            "the console belongs to this agent: hide it at startup and remove its close "
            "button. Set by install-task.ps1; never set it when running from a terminal "
            "you want to keep."
        ),
    )
    args = parser.parse_args()

    # The UI needs a console to draw in. Redirected output (`*>` in the old
    # scheduled task) and a genuinely console-less service both fail this, and
    # both must keep working rather than crashing inside Textual, so the fallback
    # is the behaviour that existed before there was a UI at all.
    interactive = not args.headless and sys.stdout.isatty()
    _configure_logging(to_console=not interactive)

    if not interactive:
        uvicorn.run(app, host=args.host, port=args.port, log_level="info")
        return

    console, api = _console_api()
    server = uvicorn.Server(
        uvicorn.Config(app, host=args.host, port=args.port, log_config=None, log_level="info")
    )
    threading.Thread(target=server.run, name="uvicorn", daemon=True).start()
    log.info("Agent listening on http://%s:%d", args.host, args.port)

    def shutdown() -> None:
        server.should_exit = True

    console.run(api, owns_console=args.hide, on_quit=shutdown)


if __name__ == "__main__":
    main()
