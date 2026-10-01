# CLAUDE.md

llama-warden: who gets the GPU, and who is told to let go of it. One systemd service on the NixOS host that measures what else is using the card, decides whether that other work is at stake, unloads the llama.cpp router, frees an idle ComfyUI, and pushes `pause`/`resume` to its consumers.

This file is rules and navigation only.

- [README.md](README.md) is how to run and configure it.
- [docs/decisions/](docs/decisions/README.md) is why anything is the way it is. **`(0001)` means `docs/decisions/0001-*.md`.** Grep it before changing something that looks arbitrary; add to it when we decide something new.
- [graphics/README.md](graphics/README.md) is what the mark means.
- [CLAUDE-TODO.md](CLAUDE-TODO.md) is what is built but not yet watched running. Read it before claiming a path works.
- [Multiple model routers](docs/decisions/0003-multiple-model-routers.md) records the second runtime, compatibility, and the limit on request coordination.
- The NixOS configuration that deploys this is `~/Projects/nixcfg` (`modules/nixos/llm.nix` for the router and the warden, `packages/comfyui` for ComfyUI).

## Current state

Linux only since 2026-09-28 (0002). The Windows program, its console, tray, launcher and preset, is in git history at `b87f39b`. What was live-verified on Windows (the announcement path into Episteme, 0001) was the push and the decision table, both unchanged by the port. The Linux probe, the router unload and the ComfyUI free are tested but not yet watched deciding anything on the real host. See [CLAUDE-TODO.md](CLAUDE-TODO.md).

## Commands

```sh
nix develop -c pytest -q               # 51 tests, no GPU, router or ComfyUI needed
nix develop -c ruff check src tests
nix develop -c ruff format --check src tests
nix build                              # the package; runs the tests too
nix develop -c python -m warden        # from the checkout, reads ./warden.toml

curl 127.0.0.1:5003/verdict            # what it decided, and who has heard it
curl 127.0.0.1:5003/resources          # a fresh NVML probe
journalctl -u llama-warden -f
```

## The rules that have to fire without being looked up

- **The machine that measures is the machine that decides** (0001). A threshold that lives in a consumer's config is the thing this project was created to end.
- **A pause is a message, not a lease.** Nothing expires, so a warden that dies while a consumer is paused leaves it paused. The mitigations are in `consumers.py` and the residual risk is stated in 0001. Do not add a second, quieter mitigation without reading that section; the real fix, if it is ever needed, is the lease.
- **An announcement must be idempotent at the other end.** The verdict is re-sent every 300 s, so a consumer that re-stamps `since` on every `pause` erases the one fact its panel shows.
- **A failed probe leaves the verdict alone, and so does an unreadable ComfyUI queue.** Neither is evidence that the GPU is free.
- **The router is unloaded on the transition only** (0002). Unloading every tick would fight a client the user chose to let through.
- **Free VRAM is only read while the router holds no model.** Linux can attribute it now, but the rule was kept, not re-decided (0002). `loading` counts as loaded (0001).
- **ComfyUI is contention, never ours.** It is not in `our_units` and it is not a consumer. It is a tenant the warden watches and frees (0002).
- **`watch` and `agent` never import each other.** `__main__` wires the probe into the loop, which is what keeps the FastAPI app testable without a GPU and the loop testable without a server.
- **The HTTP service is read-only.** Starting, stopping and configuring the servers is systemd's and nixcfg's job now.

## Engineering principles (user feedback, hard)

- **Fix root causes, not symptoms.** A consumer-side workaround is acceptable only as an explicitly temporary bridge, agreed with the user.
- **Don't self-authorize known design debt.** Surface the smell and the proper fix; the user decides.
- **Build the simplest mechanism that satisfies the stated requirement.**
- **"Tests pass" and live verification are different claims.** Say which one was done.
