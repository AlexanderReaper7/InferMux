# CLAUDE.md

llama-warden: who gets the GPU, and who is told to let go of it. One process on the Windows host that runs llama.cpp's servers, measures what else is using the card, decides whether that other work is at stake, and pushes `pause`/`resume` to its consumers.

This file is rules and navigation only.

- [README.md](README.md) is how to run and configure it.
- [docs/decisions/](docs/decisions/README.md) is why anything is the way it is. **`(0001)` means `docs/decisions/0001-*.md`.** Grep it before changing something that looks arbitrary; add to it when we decide something new.
- [graphics/README.md](graphics/README.md) is what the mark means and what the tray light maps to.
- [tools/README.md](tools/README.md) is the win32 traps, written down after each cost an hour.

## Current state

Split out of Episteme 2026-09-13, carrying `hostagent/`'s git history. What was live-verified **there** (2026-08-01, against a running Battlefield 6) was the measurement and the decision table, both of which moved unchanged. What has NOT been watched running here is the announcement path: the POST to a consumer, the re-announce, and a consumer taking it.

## Commands

```sh
uv run python -m warden             # text UI + tray icon; --headless for the bare service
uv run pytest -q                    # 81 tests, no GPU and no llama-server needed
uv run ruff check src tests tools
pwsh scripts/install-task.ps1       # the logon task (-Remove). Old name: EpistemeLlamaAgent
./scripts/open-agent.ps1            # start it, or show the running one; one door

curl http://127.0.0.1:5003/verdict  # what it decided, and who has heard it
curl http://127.0.0.1:5003/resources  # a fresh ~3.5s sweep, on purpose

# Both are GENERATED. The .ico is not a conversion of the SVG - it re-renders
# console.py's own icon_image, so the tray, the taskbar and the shortcut are one
# picture by construction.
uv run graphics/build_svg.py
uv run tools/build_ico.py
```

## The rules that have to fire without being looked up

- **The machine that measures is the machine that decides** (0001). A threshold that lives in a consumer's config is the thing this project was created to end.
- **A pause is a message, not a lease.** Nothing expires, so a warden that dies while a consumer is paused leaves it paused. The mitigations are in `consumers.py` and the residual risk is stated in 0001. Do not add a second, quieter mitigation without reading that section; the real fix, if it is ever needed, is the lease.
- **An announcement must be idempotent at the other end.** The verdict is re-sent every 300 s, so a consumer that re-stamps `since` on every `pause` erases the one fact its panel shows.
- **A failed probe leaves the verdict alone.** A probe that cannot run is not evidence that the GPU is free, and resuming on it is the one mistake this loop exists to avoid.
- **Free VRAM is only read while our own models are unloaded.** Per-process VRAM cannot be attributed on Windows: the counter reported 22 GB for dwm on a 10 GB card. `loading` counts as loaded (0001).
- **The UI measures nothing.** `AgentAPI` has `verdict`, not `resources`, and that absence is the enforcement. A probe costs ~3.5 s and the watch thread already pays it.
- **`watch` and `agent` never import each other.** `__main__` wires the probe into the loop, which is what keeps the FastAPI app testable without a GPU and the loop testable without a server.
- **Never `-WindowStyle Hidden` where `conhost --headless` is meant.** Both scripts that start the warden go through `conhost.exe <command>` on purpose: Windows Terminal's handoff leaves `GetConsoleWindow()` pointing at a window the UI does not live in, so hide-to-tray moves nothing. The comments in `scripts/install-task.ps1` carry the measurements.
- **The llama.cpp binaries are not ours.** They are an unpacked upstream release in `C:\selfhosting\llama-cpp`, reached through `LLAMA_CPP_DIR`. The launcher and `models-preset.ini` are ours and live in `llama/`.
- **The preset is edited line by line, never round-tripped through configparser.** It is half comments and those comments are the only record of why a setting is what it is.

## Engineering principles (user feedback, hard)

- **Fix root causes, not symptoms.** A consumer-side workaround is acceptable only as an explicitly temporary bridge, agreed with the user.
- **Don't self-authorize known design debt.** Surface the smell and the proper fix; the user decides.
- **Build the simplest mechanism that satisfies the stated requirement.**
- **"Tests pass" and live verification are different claims.** Say which one was done. The window makes claims pytest cannot reach, which is what `tools/` is for.
