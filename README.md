# llama-warden

Who gets the GPU, and who is told to let go of it.

One systemd service on the NixOS host. It watches what else is using the card through NVML, decides whether that other work is at stake, and acts on it. The llama.cpp router is told to unload its models, its consumers are told to pause, and ComfyUI is told to drop its models once it has sat idle. A game or a ComfyUI job is noticed within 5 seconds. The consumers resume five minutes after the GPU goes quiet.

It was Episteme's `hostagent/` until 2026-09-13 ([0001](docs/decisions/0001-the-warden-decides-and-says-so.md)) and a Windows program until 2026-09-28 ([0002](docs/decisions/0002-linux-nvml-router-unload-comfyui.md)).

## Run it

On NixOS, through the flake's module:

```nix
# flake inputs
llama-warden = {
  url = "git+https://github.com/AlexanderReaper7/llama-warden";
  inputs.nixpkgs.follows = "nixpkgs";
};

# a NixOS module
imports = [ inputs.llama-warden.nixosModules.default ];
services.llama-warden = {
  enable = true;
  settings.agent.comfyui_url = "http://127.0.0.1:8188";
};
```

From a checkout:

```sh
nix develop -c python -m warden        # reads ./warden.toml
journalctl -u llama-warden -f          # the service's log
```

## Configure it

Everything is in [`warden.toml`](warden.toml), or the module's `settings`, which is the same file as an attribute set. Absent or partial is fine, every value has a default, and a fresh clone runs with no consumers and therefore nothing to announce to.

```toml
[agent]
router_url = "http://127.0.0.1:5001"   # unloaded on a yield
additional_router_urls = ["http://127.0.0.1:5004"] # optional second runtime
comfyui_url = "http://127.0.0.1:8188"  # omit when there is no ComfyUI
our_units = ["llama-cpp.service", "llama-embed.service"]

[policy]
gpu_busy_percent = 25.0       # foreign GPU load at or above this is contention
min_free_vram_mb = 3000       # only read while the router holds no model
resume_quiet_seconds = 300    # yield at once, come back slowly
poll_seconds = 5.0
comfyui_idle_seconds = 600    # empty queue this long, then POST /free

[[consumers]]
name = "episteme"
url = "http://127.0.0.1:8200"
```

## Talk to it

Loopback :5003, no auth, read-only.

```sh
curl http://127.0.0.1:5003/verdict      # what it decided, who has heard it, ComfyUI's idle clock
curl http://127.0.0.1:5003/status       # which services are listening, the router's models
curl http://127.0.0.1:5003/resources    # a fresh NVML probe, on purpose
```

## What it does on a yield

1. `POST /models/unload` to the router for each model it holds. Once, on the transition. A client that asks for a model during the pause loads it again.
2. One POST to every configured consumer, repeated every five minutes until it lands:

```json
{"action": "pause", "reason": "ComfyUI has 1 job queued",
 "since": "2026-09-28T18:04:11+00:00", "warden": "llama-warden"}
```

What a consumer does about that is the consumer's business. **The endpoint must be idempotent**: re-announcing `pause` to an already-paused consumer must not re-stamp when the pause began.

Nothing expires. A warden that dies while a consumer is paused leaves it paused, and that trade is argued in [0001](docs/decisions/0001-the-warden-decides-and-says-so.md#push-not-a-lease).

## Layout

| path | what it is |
|---|---|
| `src/warden/policy.py` | the decision, and ComfyUI's idle clock. Pure functions. |
| `src/warden/watch.py` | the loop: measure, decide, unload, free, announce. One thread. |
| `src/warden/probe.py` | NVML, and which processes are ours by their cgroup. |
| `src/warden/router.py` | the llama.cpp router: does it hold a model, unload them all. |
| `src/warden/comfyui.py` | ComfyUI: queue depth, `/free`. |
| `src/warden/consumers.py` | the push, and what each consumer was last known to accept. |
| `src/warden/agent.py` | the read-only HTTP service. |
| `nix/` | the package and the NixOS module. |
| `graphics/` | the mark, and the generator that emits it. |

## Develop

```sh
nix develop -c pytest -q
nix develop -c ruff check src tests
nix develop -c ruff format --check src tests
nix build                              # runs the tests as part of the build
```
