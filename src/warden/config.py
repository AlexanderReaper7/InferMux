"""Where the thresholds and the addresses live.

One TOML file, read once at import. `WARDEN_CONFIG` names it; the NixOS module
sets that to a file it generates, and a checkout falls back to `warden.toml`
beside the repository root. TOML rather than environment variables because the
consumer list is a list of tables, and a list of tables does not survive
`KEY=value`. `tomllib` is in the standard library, so this costs no dependency.

**Every threshold that used to sit in Episteme's settings is here** (0001).
That is the whole point of the split: the machine that measures the GPU is the
machine that decides, and a reader who wants to know why the pipeline stopped
has one file to look at rather than two projects to correlate.

The llama.cpp servers are not the warden's to run any more. systemd runs them
(`llama-cpp.service` and `llama-embed.service` in the NixOS configuration), and
the warden only talks to the router over HTTP (0002).
"""

from __future__ import annotations

import os
import tomllib
from dataclasses import dataclass, field
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
    within one tick. On Windows a tick cost ~3.5 s of PowerShell and PDH
    sampling, so it was 30. NVML answers in ~6 ms (measured 2026-09-28 on the
    3080), which makes a short tick nearly free.

    `min_free_vram_mb` is only read while the router holds no model. The
    Windows value was 6000. On this Linux desktop the compositor, the browser
    and the Electron apps hold 4.2 to 4.7 GB of the 10 GB between them, so 6000
    would read an idle desktop as contended and never resume.

    `comfyui_idle_seconds` is how long ComfyUI's queue stays empty before the
    warden asks it to drop its models (0002).
    """

    gpu_busy_percent: float = 25.0
    min_free_vram_mb: int = 3000
    resume_quiet_seconds: int = 300
    poll_seconds: float = 5.0
    comfyui_idle_seconds: int = 600
    enabled: bool = True


@dataclass(frozen=True)
class Settings:
    host: str
    port: int
    policy: Policy
    consumers: tuple[Consumer, ...]
    # The router's own address: asked whether it holds a model, and told to
    # unload them all when the warden yields.
    router_url: str = "http://127.0.0.1:5001"
    # None when there is no ComfyUI to watch.
    comfyui_url: str | None = None
    # The systemd units whose GPU work is ours, not contention. Matched against
    # the last component of /proc/<pid>/cgroup.
    our_units: tuple[str, ...] = field(default=("llama-cpp.service", "llama-embed.service"))
    additional_router_urls: tuple[str, ...] = ()

    @property
    def router_urls(self) -> tuple[str, ...]:
        """Every router, once each; keep router_url as the primary for existing clients."""
        return tuple(
            dict.fromkeys(
                url.rstrip("/") for url in (self.router_url, *self.additional_router_urls)
            )
        )


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
    agent = data.get("agent") or {}
    units = agent.get("our_units")
    return Settings(
        host=agent.get("host", "127.0.0.1"),
        port=int(agent.get("port", 5003)),
        router_url=agent.get("router_url", "http://127.0.0.1:5001"),
        additional_router_urls=tuple(agent.get("additional_router_urls") or ()),
        comfyui_url=agent.get("comfyui_url") or None,
        our_units=tuple(units) if units else Settings.__dataclass_fields__["our_units"].default,
        policy=policy,
        consumers=consumers,
    )


settings = load()
