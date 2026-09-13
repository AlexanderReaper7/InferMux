# llama-warden

Who gets the GPU, and who is told to let go of it.

One process on the Windows host. It runs llama.cpp's servers, watches what else is using the card, decides whether that other work is at stake, and tells its consumers to pause or resume. A game starting is noticed within 30 seconds; the pipeline that was writing articles stops, hands back VRAM, and comes back five minutes after the game is gone.

It was Episteme's `hostagent/` until 2026-09-13. The policy came with it ([0001](docs/decisions/0001-the-warden-decides-and-says-so.md)).

## Run it

```pwsh
uv run python -m warden                 # text UI + tray icon
uv run python -m warden --headless      # the bare service
pwsh scripts/install-task.ps1           # at logon, hidden, tray icon (-Remove)
pwsh scripts/install-shortcut.ps1       # Start menu entry (-Desktop, -Remove)
./scripts/open-agent.ps1                # start it, or show the running one
```

The console is one window with three tabs: the router's log, the embedder's log, and the warden's own. It is born hidden behind the tray icon, which doubles as a status light. The icon's **nodes are the decode server** (:5001) and its **edges are the embedder** (:5002); grey is down, never red.

## Configure it

Everything is in [`warden.toml`](warden.toml): where llama.cpp is unpacked, the four policy numbers, and one table per consumer. Absent or partial is fine, every value has a default, and a fresh clone runs with no consumers and therefore nothing to announce to.

```toml
[policy]
gpu_busy_percent = 25.0       # foreign GPU load at or above this is contention
min_free_vram_mb = 6000       # only read while our own models are unloaded
resume_quiet_seconds = 300    # yield at once, come back slowly
poll_seconds = 30.0

[[consumers]]
name = "episteme"
url = "http://127.0.0.1:8200"
```

## Talk to it

Loopback :5003, no auth.

```sh
curl http://127.0.0.1:5003/verdict          # what it decided, and who has heard it
curl http://127.0.0.1:5003/status           # ports, PIDs, uptime
curl http://127.0.0.1:5003/resources        # a fresh ~3.5s sweep, on purpose
curl -X POST http://127.0.0.1:5003/start    # idempotent; /stop, /restart
curl "http://127.0.0.1:5003/logs?which=router&tail=200"
```

## What it tells a consumer

One POST per transition, to every configured consumer, repeated every five minutes until it lands:

```json
{"action": "pause", "reason": "foreign GPU load 91% >= 25% (bf6)",
 "since": "2026-09-13T18:04:11+00:00", "warden": "llama-warden"}
```

What a consumer does about that is the consumer's business. **The endpoint must be idempotent**: re-announcing `pause` to an already-paused consumer must not re-stamp when the pause began.

Nothing expires. A warden that dies while a consumer is paused leaves it paused, and that trade is argued in [0001](docs/decisions/0001-the-warden-decides-and-says-so.md#push-not-a-lease) rather than glossed over.

## Layout

| path | what it is |
|---|---|
| `src/warden/policy.py` | the decision. Pure functions over a measurement dictionary. |
| `src/warden/watch.py` | the loop: measure, decide, announce. One thread. |
| `src/warden/consumers.py` | the push, and what each consumer was last known to accept. |
| `src/warden/agent.py` | the actuator: start/stop/restart, logs, the probe, the preset. |
| `src/warden/console.py` | the tray icon and the text UI. Measures nothing. |
| `llama/` | the launcher and `models-preset.ini`, versioned. The binaries are not. |
| `tools/` | win32 instruments for the claims pytest cannot reach. Read its README. |
| `graphics/` | the mark, and the generator that emits it. |

The llama.cpp binaries stay in `C:\selfhosting\llama-cpp` as an unpacked upstream release, reached through `LLAMA_CPP_DIR`. The launcher is ours and lives here.

## Develop

```sh
uv sync
uv run pytest -q
uv run ruff check src tests tools
uv run graphics/build_svg.py    # the mark
uv run tools/build_ico.py       # the .ico, re-rendered from console.py's own drawing
```
