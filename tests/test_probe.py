"""The measurement (`warden/probe.py`), without a GPU.

NVML itself is not called here. What is tested is what the probe does with its
answers: whose work a process is, what it is called, and the sums the policy
reads. That is where a mistake would pause the router for its own generation.
"""

from pathlib import Path

from warden.probe import process_name, process_unit, summarise

OURS = frozenset({"llama-cpp.service", "llama-embed.service"})


def fake_proc(root: Path, pid: int, *, argv0: str = "", comm: str = "", cgroup: str = "") -> None:
    d = root / str(pid)
    d.mkdir(parents=True)
    (d / "cmdline").write_bytes(argv0.encode() + b"\0--flag\0" if argv0 else b"")
    (d / "comm").write_text(comm + "\n")
    (d / "cgroup").write_text(cgroup + "\n")


def test_the_nix_wrapper_disguise_is_taken_off(tmp_path):
    """Measured 2026-09-28: comm read `.blender-wrappe` for the render."""
    fake_proc(tmp_path, 7, argv0="/nix/store/abc-blender/bin/.blender-wrapped")
    assert process_name(7, tmp_path) == "blender"


def test_a_rewritten_chromium_argv_keeps_only_the_name(tmp_path):
    """Electron apps put their flags into argv[0] itself, spaces and all."""
    fake_proc(tmp_path, 12, argv0="T3 Code (Alpha) --gpu-preferences=YAAA --shared-files")
    fake_proc(tmp_path, 13, argv0="vesktop --type=gpu-process")
    assert process_name(12, tmp_path) == "T3 Code (Alpha)"
    assert process_name(13, tmp_path) == "vesktop"


def test_a_process_with_no_cmdline_falls_back_to_comm(tmp_path):
    """Kernel threads and some zombies have an empty cmdline."""
    fake_proc(tmp_path, 8, comm="Xwayland")
    assert process_name(8, tmp_path) == "Xwayland"


def test_a_vanished_process_is_unknown_not_an_error(tmp_path):
    """The process list and /proc are read at different moments."""
    assert process_name(9, tmp_path) == "unknown"
    assert process_unit(9, tmp_path) is None


def test_the_unit_is_the_last_cgroup_component(tmp_path):
    fake_proc(tmp_path, 10, cgroup="0::/system.slice/llama-cpp.service")
    fake_proc(tmp_path, 11, cgroup="0::/user.slice/user-1000.slice/session-4.scope")
    assert process_unit(10, tmp_path) == "llama-cpp.service"
    assert process_unit(11, tmp_path) == "session-4.scope"


def _summary(vram, util, units, names=None):
    return summarise(
        vram=vram,
        util=util,
        used_mb=6000,
        total_mb=10240,
        gpu_percent=99,
        name=lambda pid: (names or {}).get(pid, f"p{pid}"),
        unit=lambda pid: units.get(pid),
        our_units=OURS,
    )


def test_our_generation_is_split_from_everyone_elses():
    """The whole policy rests on this split: our own generation pins the GPU at
    ~100%, so a figure that included it could never mean 'someone else needs the
    card'."""
    result = _summary(
        vram={1: 3000, 2: 5000, 3: 1600},
        util={1: 90.0, 2: 45.0, 3: 0.5},
        units={1: "session-4.scope", 2: "llama-cpp.service", 3: "session-4.scope"},
        names={1: "bf6", 2: "llama-server", 3: "niri"},
    )
    assert result["foreign_gpu_percent"] == 90.5
    assert result["our_gpu_percent"] == 45.0
    assert result["our_vram_mb"] == 5000
    assert result["vram_free_mb"] == 4240
    assert result["culprits"] == ["bf6", "niri"]
    assert [p["name"] for p in result["processes"]][:2] == ["bf6", "llama-server"]


def test_comfyui_is_not_ours():
    """Its work competes with the router's for the same 10 GB."""
    result = _summary(vram={5: 8000}, util={5: 97.0}, units={5: "comfyui.service"})
    assert result["foreign_gpu_percent"] == 97.0
    assert result["our_vram_mb"] == 0


def test_memory_without_work_is_not_a_culprit():
    """The compositor holds 1.6 GB and draws nothing. Naming it in a verdict
    would send the reader after the wrong process."""
    result = _summary(vram={3: 1672}, util={}, units={3: "session-4.scope"})
    assert result["culprits"] == []
    assert result["processes"][0]["vram_mb"] == 1672
