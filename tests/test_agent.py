"""The actuator (`warden/agent.py`): lifecycle, logs, the probe, the preset.

Its PowerShell calls are stubbed. What is worth testing is the logic wrapped
around them, which is where the live defects actually were.

Carried over from Episteme's `tests/test_hostagent.py` at the split, with the
by-path module loading replaced by an ordinary import: the agent is a module in
a package now, not a script loaded from outside one.
"""

import threading
import time

import pytest
from fastapi import HTTPException

from warden import agent as agent_module


@pytest.fixture
def agent():
    """One module, restored between tests by `monkeypatch`, which is what every
    test here mutates it through."""
    return agent_module


def test_ansi_escapes_are_stripped_from_log_lines(agent, tmp_path, monkeypatch):
    """llama-server colors its output and `--log-file` receives the escape codes
    verbatim, so an unfiltered pane renders "\\x1b[34m0.00.193\\x1b[0m" as noise.
    Found live on the first real start."""
    monkeypatch.setattr(agent, "LOG_DIR", tmp_path)
    (tmp_path / "router.log").write_text(
        "\x1b[34m0.00.193.237\x1b[0m \x1b[32mI\x1b[0m srv listening on http://127.0.0.1:5001\n",
        encoding="utf-8",
    )
    lines = agent.read_log(which="router", tail=10)["lines"]
    assert lines == ["0.00.193.237 I srv listening on http://127.0.0.1:5001"]


def test_the_doubled_carriage_return_is_not_a_blank_line(agent, tmp_path, monkeypatch):
    """Every line llama-server writes on Windows ends "\\r\\r\\n": it emits CRLF and
    the CRT translates the \\n a second time. splitlines() reads the orphan \\r as
    a break, so an unfiltered pane is double-spaced - found live 2026-08-15 in
    the agent console, and present in /admin's pane for as long as it has existed.

    A LONE \\r still splits: llama.cpp rewrites progress in place with it."""
    monkeypatch.setattr(agent, "LOG_DIR", tmp_path)
    _write(
        tmp_path / "router.log",
        "srv listening\r\r\nque start_loop\r\r\nload 10%\rload 90%\r\r\n",
    )
    assert agent.read_log(which="router", tail=10)["lines"] == [
        "srv listening",
        "que start_loop",
        "load 10%",
        "load 90%",
    ]


def test_log_tail_reads_a_bounded_window_of_a_huge_file(agent, tmp_path, monkeypatch):
    """A log that grew overnight must cost a fixed read, not a full slurp — the
    tail seeks from the end. The partial line the seek lands mid-way through is
    dropped rather than shown truncated."""
    monkeypatch.setattr(agent, "LOG_DIR", tmp_path)
    (tmp_path / "router.log").write_text(
        "".join(f"line {i:06d} {'x' * 300}\n" for i in range(20_000)), encoding="utf-8"
    )
    result = agent.read_log(which="router", tail=5)
    assert len(result["lines"]) == 5
    assert result["lines"][-1].startswith("line 019999")
    assert all(not line.startswith("ine") for line in result["lines"])


def _write(path, text):
    with path.open("a", encoding="utf-8", newline="") as handle:
        handle.write(text)


def test_delta_read_returns_only_what_was_appended(agent, tmp_path, monkeypatch):
    """The whole point of the offset protocol: a live pane appends what arrived
    instead of re-fetching a 300-line tail every few seconds, which is both ~99%
    less data and the only way the reader's text selection and scrollback in the
    pane survive an update."""
    monkeypatch.setattr(agent, "LOG_DIR", tmp_path)
    path = tmp_path / "router.log"
    _write(path, "first\nsecond\n")

    tail = agent.read_log(which="router")
    assert tail["lines"] == ["first", "second"]

    # Nothing new: an empty answer, and the offset does not move.
    idle = agent.read_log(which="router", since=tail["next_offset"])
    assert idle["lines"] == [] and idle["next_offset"] == tail["next_offset"]

    _write(path, "third\n")
    delta = agent.read_log(which="router", since=idle["next_offset"])
    assert delta["lines"] == ["third"]
    assert delta["reset"] is False
    assert delta["next_offset"] == delta["size_bytes"]


def test_a_half_written_line_is_withheld_until_it_is_complete(agent, tmp_path, monkeypatch):
    """The server is writing while we read. Shipping the bytes so far would
    render a truncated line, and — worse — advancing past them would make the
    rest of that line arrive as a line of its own."""
    monkeypatch.setattr(agent, "LOG_DIR", tmp_path)
    path = tmp_path / "router.log"
    _write(path, "done\n")
    start = agent.read_log(which="router")["next_offset"]

    _write(path, "half a li")
    partial = agent.read_log(which="router", since=start)
    assert partial["lines"] == []
    assert partial["next_offset"] == start  # stayed put, deliberately

    _write(path, "ne\n")
    assert agent.read_log(which="router", since=partial["next_offset"])["lines"] == ["half a line"]


def test_a_truncated_log_tells_the_caller_to_reset(agent, tmp_path, monkeypatch):
    """The launcher truncates the log on every start, so an offset held across a
    restart points into the middle of a different file. Appending onto it would
    splice the new run's output into the old run's, with a mangled first line."""
    monkeypatch.setattr(agent, "LOG_DIR", tmp_path)
    path = tmp_path / "router.log"
    _write(path, "old run, many lines\n" * 50)
    stale = agent.read_log(which="router")["next_offset"]

    path.write_text("new run\n", encoding="utf-8")
    after = agent.read_log(which="router", since=stale)
    assert after["reset"] is True
    assert after["lines"] == ["new run"]
    assert after["gap_bytes"] == 0  # nothing was skipped, the file simply restarted


def test_a_backlog_past_the_window_resets_and_measures_the_gap(agent, tmp_path, monkeypatch):
    """A delta read is bounded by the same window as a tail read — a pane left
    open through a noisy night must not pull the whole file. The caller is told
    to replace rather than append, and how much it missed, so it can admit the
    discontinuity instead of quietly splicing two distant parts of the log."""
    monkeypatch.setattr(agent, "LOG_DIR", tmp_path)
    path = tmp_path / "router.log"
    _write(path, "".join(f"line {i:06d} {'x' * 300}\n" for i in range(2_000)))

    result = agent.read_log(which="router", tail=5, since=0)
    assert result["reset"] is True
    assert len(result["lines"]) == 5
    assert result["lines"][-1].startswith("line 001999")
    assert result["gap_bytes"] == result["size_bytes"] - 5 * 400
    # The tail-read rule still holds: no truncated line at the head of the window.
    assert all(line.startswith("line ") for line in result["lines"])


def test_unknown_log_is_404(agent):
    from fastapi import HTTPException

    with pytest.raises(HTTPException) as exc:
        agent.read_log(which="../../etc/passwd")
    assert exc.value.status_code == 404


def test_missing_log_reports_absence_rather_than_failing(agent, tmp_path, monkeypatch):
    """Expected before the first start after the launcher gained --log-file."""
    monkeypatch.setattr(agent, "LOG_DIR", tmp_path)
    assert agent.read_log(which="router") == {
        "log": "router",
        "path": str(tmp_path / "router.log"),
        "exists": False,
        "lines": [],
        "next_offset": 0,
        "reset": False,
    }
    # A caller holding an offset from before a restart must DROP what it shows,
    # not sit waiting to append onto a file that no longer exists.
    assert agent.read_log(which="router", since=900)["reset"] is True


def test_concurrent_starts_launch_exactly_once(agent, monkeypatch):
    """Regression: a bare "is it listening?" check is a TOCTOU race and loses it.
    Measured live — two requests 300ms apart both saw nothing listening, both ran
    the launcher, and FOUR llama-server processes came up. The launcher's own
    Get-NetTCPConnection guard has the identical hole, so serializing here is the
    only thing that closes it.

    The lock must cover the port wait, not just the check: releasing it after
    spawning would let the next caller observe the not-yet-bound port and launch
    a second server anyway."""
    launches = []
    started = threading.Event()

    def fake_run_ps(script, timeout=30.0):
        if "launch-llama" in script:
            launches.append(script)
            time.sleep(0.2)  # the window the race used to open
            started.set()
        return type("R", (), {"returncode": 0, "stdout": "", "stderr": ""})()

    monkeypatch.setattr(agent, "_run_ps", fake_run_ps)
    # Ports report down until the launcher has run, then up — the real sequence.
    monkeypatch.setattr(agent, "_port_open", lambda port, timeout=0.5: started.is_set())

    results = []
    threads = [
        threading.Thread(target=lambda: results.append(agent.start_servers())) for _ in range(4)
    ]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join(timeout=30)

    assert len(launches) == 1, f"launcher ran {len(launches)} times; must be exactly 1"
    assert sum(1 for r in results if r["started"]) == 1
    assert all(r["reason"] == "already running" for r in results if not r["started"])


def test_start_reports_failure_when_a_server_never_binds(agent, monkeypatch):
    """A zero exit code is not evidence that the servers came up. PowerShell's
    default $ErrorActionPreference is Continue, so a launcher that failed on a
    missing model or a bad preset writes its complaint to the console and still
    exits 0 — which used to be reported as `started: true`, sending the operator
    to a working-looking panel while the one thing that explains it (the captured
    output) was thrown away. The ports are the actual claim."""
    from fastapi import HTTPException

    monkeypatch.setattr(
        agent,
        "_run_ps",
        lambda script, timeout=30.0: type(
            "R", (), {"returncode": 0, "stdout": "model file not found: qwopus.gguf", "stderr": ""}
        )(),
    )
    monkeypatch.setattr(agent, "_port_open", lambda port, timeout=0.5: False)
    monkeypatch.setattr(
        agent,
        "_wait_for_ports",
        lambda **kwargs: {"router": {"listening": False}, "embed": {"listening": True}},
    )

    with pytest.raises(HTTPException) as exc:
        agent.start_servers()
    assert exc.value.status_code == 500
    assert "router" in exc.value.detail
    assert "model file not found" in exc.value.detail  # the reason survives


def test_status_skips_the_subprocess_when_nothing_is_listening(agent, monkeypatch):
    """The backend-is-down path is exactly when someone is staring at the admin
    page, so it must not pay for a PowerShell round-trip to describe processes
    that do not exist."""
    calls = []
    monkeypatch.setattr(agent, "_port_open", lambda port, timeout=0.5: False)
    monkeypatch.setattr(agent, "_run_ps", lambda *a, **kw: calls.append(a) or None)

    rows = agent._server_status()
    assert calls == []
    assert set(rows) == {"router", "embed"}
    assert all(row["listening"] is False and row["pid"] is None for row in rows.values())


def test_resources_separates_our_gpu_load_from_everyone_elses(agent, monkeypatch):
    """The whole policy rests on this split: our own generation pins the GPU at
    ~100%, so a figure that included it could never mean 'someone else needs the
    card'."""
    monkeypatch.setattr(
        agent,
        "_run_ps",
        lambda *a, **kw: type("R", (), {"returncode": 0, "stderr": "", "stdout": (
            '{"processes":['
            '{"pid":1,"name":"bf6","percent":90.0,"ours":false},'
            '{"pid":2,"name":"llama-server","percent":45.0,"ours":true},'
            '{"pid":3,"name":"Code","percent":0.5,"ours":false}],'
            '"vram":{"used_mb":9000,"total_mb":10240,"utilization":99},'
            '"running":["bf6","Code"]}'
        )})(),
    )
    monkeypatch.setattr(agent, "_known_games", lambda: {"bf6", "Anno1800"})

    result = agent.resources()
    assert result["foreign_gpu_percent"] == 90.5
    assert result["our_gpu_percent"] == 45.0
    assert result["vram_free_mb"] == 1240
    # Only games actually running, not the whole installed catalogue.
    assert result["games_running"] == ["bf6"]
    # Busiest first, so the panel shows who is using the GPU.
    assert [p["name"] for p in result["processes"]][:2] == ["bf6", "llama-server"]


# --- the model configuration: applied, never chosen ---------------------------------------

PRESET_TEXT = """\
; models-preset.ini - per-model llama-server flags.
; Sections are keyed by GGUF name minus the extension.

[Qwen3-35B-A3B.Q4_K_M]
; 2026-08-15: `ngl = 999` was removed here. `fit = on` sizes the offload against
; free VRAM; a hard ngl overrode it and cost 3x throughput for months.
fit = on
ctx = 32768

[Octen-Embedding-4B.Q8_0]
n-gpu-layers = 0
"""


def test_a_preset_edit_preserves_every_comment(agent):
    """models-preset.ini is half comments, and those comments are the only record
    of why a setting is what it is. A configparser round-trip would delete more
    knowledge in one call than the whole benchmarking feature produces."""
    text, changed = agent._edit_preset(
        PRESET_TEXT, {"Qwen3-35B-A3B.Q4_K_M": {"ctx": 16384}}
    )
    assert "cost 3x throughput for months" in text
    assert text.count(";") == PRESET_TEXT.count(";")
    assert "ctx = 16384" in text and "ctx = 32768" not in text
    assert changed == ["[Qwen3-35B-A3B.Q4_K_M] ctx: 32768 -> 16384"]


def test_a_new_key_lands_inside_its_own_section(agent):
    text, changed = agent._edit_preset(PRESET_TEXT, {"Qwen3-35B-A3B.Q4_K_M": {"batch": 4096}})
    sections = agent._parse_preset(text)
    assert sections["Qwen3-35B-A3B.Q4_K_M"]["batch"] == "4096"
    assert "batch" not in sections["Octen-Embedding-4B.Q8_0"]
    assert changed == ["[Qwen3-35B-A3B.Q4_K_M] batch = 4096 (added)"]


def test_editing_an_earlier_section_does_not_corrupt_a_later_one(agent):
    """Inserting a line shifts every index after it. Applied bottom-up so a
    section's recorded bounds are still valid when its turn comes."""
    text, _ = agent._edit_preset(
        PRESET_TEXT,
        {
            "Qwen3-35B-A3B.Q4_K_M": {"batch": 4096},
            "Octen-Embedding-4B.Q8_0": {"threads": 8},
        },
    )
    sections = agent._parse_preset(text)
    assert sections["Qwen3-35B-A3B.Q4_K_M"] == {"fit": "on", "ctx": "32768", "batch": "4096"}
    assert sections["Octen-Embedding-4B.Q8_0"] == {"n-gpu-layers": "0", "threads": "8"}


def test_removing_a_key_comments_it_out_rather_than_deleting_the_line(agent):
    """A sweep that silently vanished a line is the exact failure the line-based
    writer exists to avoid, and the restore still puts the original file back."""
    text, changed = agent._edit_preset(PRESET_TEXT, {"Qwen3-35B-A3B.Q4_K_M": {"fit": None}})
    assert "; [benchmark] fit = on" in text
    assert "fit" not in agent._parse_preset(text)["Qwen3-35B-A3B.Q4_K_M"]
    assert changed == ["[Qwen3-35B-A3B.Q4_K_M] removed fit (was fit = on)"]


def test_a_section_that_does_not_exist_yet_is_created(agent):
    text, _ = agent._edit_preset(PRESET_TEXT, {"New-Model.Q8_0": {"ctx": 8192}})
    assert agent._parse_preset(text)["New-Model.Q8_0"] == {"ctx": "8192"}


def test_an_unchanged_value_is_not_reported_as_a_change(agent):
    _, changed = agent._edit_preset(PRESET_TEXT, {"Qwen3-35B-A3B.Q4_K_M": {"fit": "on"}})
    assert changed == []


def test_launcher_arguments_cannot_become_powershell(agent):
    """They are interpolated into a command line assembled by string joining.
    Not a trust boundary against Episteme, which can already start processes
    here - but that command line must not be one quote away from arbitrary
    execution."""
    assert agent._safe_args(["--ctx-size", "32768", "-ngl", "999"]) == [
        "--ctx-size", "32768", "-ngl", "999"
    ]
    for hostile in ("'; rm -rf /", "$(whoami)", "`whoami`", "a;b", 5):
        with pytest.raises(HTTPException):
            agent._safe_args([hostile])


def test_a_backup_path_cannot_escape_the_preset_directory(agent, tmp_path, monkeypatch):
    """The warden binds loopback only, but a path parameter that writes over
    arbitrary files is not something to leave resting on the network boundary."""
    monkeypatch.setattr(agent, "PRESET", tmp_path / "models-preset.ini")
    good = tmp_path / f"{agent.PRESET_BACKUP_PREFIX}20260815-120000"
    good.write_text("x", encoding="utf-8")
    assert agent._backup_path(good.name) == good.resolve()
    for hostile in ("../../etc/passwd", "models-preset.ini", r"C:\Windows\system.ini"):
        with pytest.raises(HTTPException):
            agent._backup_path(hostile)
