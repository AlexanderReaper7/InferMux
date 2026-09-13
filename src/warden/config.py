"""Where the thresholds and the paths live.

One TOML file, `warden.toml` beside the repository root, read once at import.
TOML rather than environment variables because the consumer list is a list of
tables, and a list of tables does not survive `KEY=value`. `tomllib` is in the
standard library, so this costs no dependency.

**Every threshold that used to sit in Episteme's settings is here** (0001).
That is the whole point of the split: the machine that measures the GPU is the
machine that decides, and a reader who wants to know why the pipeline stopped
has one file to look at rather than two projects to correlate.

The llama.cpp *binaries* are not ours and are not in this repository. `llama_dir`
points at the unpacked upstream release, which also owns the logs the servers
write; `LLAMA_CPP_DIR` overrides it for a second unpack. The launcher and the
preset it reads travel with this repository, in `llama/`.
"""

from __future__ import annotations

import os
import tomllib
from dataclasses import dataclass
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
CONFIG_PATH = Path(os.environ.get("WARDEN_CONFIG") or ROOT / "warden.toml")


@dataclass(frozen=True)
class Consumer:
    """Something that yields the GPU when told to.

    `url` is the base of an HTTP service, `announce_path` the route that takes
    `{"action": "pause"|"resume", "reason": "..."}`. Nothing else is assumed
    about it: the warden decides that the GPU is contended, and what a consumer
    does about that is the consumer's own business. Episteme pauses its pipeline
    and hands back VRAM; a second consumer might do something else entirely.
    """

    name: str
    url: str
    announce_path: str = "/api/pipeline/announce"
    timeout_seconds: float = 10.0

    @property
    def endpoint(self) -> str:
        return f"{self.url.rstrip('/')}{self.announce_path}"


@dataclass(frozen=True)
class Policy:
    """The decision table's numbers. See `policy.decide` for what each one does.

    `poll_seconds` is the yield latency: a game that starts now is noticed
    within one tick. It is not free — a tick shells out to PowerShell for the
    per-process GPU counter, whose PDH sampling floor makes it ~3.5s — so this
    is a deliberate trade of host CPU for how long Episteme keeps a 20 GB model
    resident after somebody alt-tabs into a game. Under Episteme's old */2 cron
    the answer was "up to two minutes".
    """

    gpu_busy_percent: float = 25.0
    min_free_vram_mb: int = 6000
    resume_quiet_seconds: int = 300
    poll_seconds: float = 30.0
    enabled: bool = True


@dataclass(frozen=True)
class Settings:
    llama_dir: Path
    models_dir: Path
    host: str
    port: int
    policy: Policy
    consumers: tuple[Consumer, ...]
    # The router's own address, asked whether it is holding a model in VRAM.
    # Free VRAM is only readable as *someone else's* while we hold nothing, so
    # this answer decides whether the memory half of the policy applies at all.
    router_url: str = "http://127.0.0.1:5001"

    @property
    def launcher(self) -> Path:
        return ROOT / "llama" / "launch-llama-v2.ps1"

    @property
    def preset(self) -> Path:
        return ROOT / "llama" / "models-preset.ini"

    @property
    def log_dir(self) -> Path:
        return self.llama_dir / "logs"


def _load(path: Path) -> dict:
    if not path.exists():
        return {}
    with path.open("rb") as handle:
        return tomllib.load(handle)


def load(path: Path | None = None) -> Settings:
    """Read the file. Absent or partial is fine: every value has a default, so a
    fresh clone runs before anybody has configured anything, with no consumers
    and therefore nothing to announce to."""
    data = _load(path or CONFIG_PATH)
    policy = Policy(**(data.get("policy") or {}))
    consumers = tuple(Consumer(**row) for row in (data.get("consumers") or []))
    paths = data.get("paths") or {}
    llama_dir = Path(
        os.environ.get("LLAMA_CPP_DIR") or paths.get("llama_dir") or r"C:\selfhosting\llama-cpp"
    )
    agent = data.get("agent") or {}
    return Settings(
        llama_dir=llama_dir,
        models_dir=Path(paths.get("models_dir") or r"C:\selfhosting\models"),
        host=agent.get("host", "127.0.0.1"),
        port=int(agent.get("port", 5003)),
        router_url=agent.get("router_url", "http://127.0.0.1:5001"),
        policy=policy,
        consumers=consumers,
    )


settings = load()

# Server names must match the launcher's -LogFile naming and its port constants.
SERVERS = {"router": 5001, "embed": 5002}
PROCESS_NAME = "llama-server"
PRESET_BACKUP_PREFIX = "models-preset.ini.bak-"

LLAMA_DIR = settings.llama_dir
LAUNCHER = settings.launcher
LOG_DIR = settings.log_dir
PRESET = settings.preset
