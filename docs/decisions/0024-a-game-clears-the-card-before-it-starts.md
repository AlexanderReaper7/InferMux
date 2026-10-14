# 0024. A game clears the card before it starts, through a keyless control socket

- Date: 2026-10-12
- Status: accepted; built and tested 2026-10-12, running on strix by 2026-10-14 (the socket is `infermux:users` 0660 and answers the user without a key); no launch through `infermux-hold` watched yet
- Keeps [0004](0004-infermux-request-priority.md) (an interactive request in flight is never unloaded under) and [0017](0017-a-priority-process-takes-the-card.md) (reloads during a yield stay allowed).

## Context

On 2026-10-12 Warframe ran at 2-5 fps on strix: in-game GPU time 496-539 ms, the card 100% busy at 174 W of 340 W with 4% memory-controller use. The cause was in the journal and EE.log:

- 19:45:20, InferMux loaded Qwopus3.6-35B on the 3080 for Episteme.
- About 19:50:20, Warframe started. At t=3.1 s it read its VRAM budget: `Local: Budget = 267714560`, 255 MB of 10 GB. Between t=3.7 and t=5.6 s it created every render target, then logged `Only 239MB / 10GB video memory free` and set texture streaming to `MINIMIZED (Budget: 0B)`.
- 19:50:26, the warden measured Warframe at 28% and paused; 19:50:27 the 35B unloaded.

The game never recreates its render targets (no `ResizeBuffers` after t=3.1 s), so what it placed outside VRAM stayed there for the whole run. Relaunched at 20:23 with 7.4 GB free and nothing else changed (same gamescope launch options, fullscreen and focused), it read a 7.36 GB budget and ran at 97 fps with 9.39 ms of GPU time.

The warden cannot see this coming. It polls every 5 s and counts load only at 25%, and a game reads its budget before it creates any. `priority_processes` (0017) does not help either: it matches NVML's compute list, and a Proton game is in the graphics list only. Anything that waits for the game to appear on the card is too late.

## Decision

The user, 2026-10-12, each asked in turn.

1. **The game's launch clears the card first.** `infermux-hold -- <command>` sets a manual pause, unloads the models, waits until InferMux holds no VRAM, then runs the command. When the command exits it sets `auto`. As a Steam launch option: `infermux-hold -- gamescope ... -- %command%`. Rejected: noticing the game by process name, which is still after the budget is read.
2. **A command of InferMux's, not a script in nixcfg.** It ships and is tested with the API it calls.
3. **No key.** The daemon also listens on a Unix socket, `-control-socket`, mode 0660 and owned by `-control-group`, that answers only `/warden/verdict`, `/warden/resources` and the controls (`/warden/manual`, `unload`, `forgive`, `cancel-batch`, `comfyui/free`), without a key. File permissions are the authorization. Rejected: exempting loopback, because `tailscale serve` forwards tailnet requests to the daemon from 127.0.0.1, so any tailnet device could have paused the warden and unloaded models. Also rejected: a key file for the command.
4. **A card that will not come clear refuses the launch.** The unload is refused while an interactive request is in flight (0004), so it is tried every second for `-timeout` (30 s). If the card is still not clear, the pause is handed back, the reason goes to stderr and `notify-send`, and the command does not start. Rejected: launching anyway with a warning, which gives exactly the 2 fps run this exists to prevent.

How it is built (the agent's choice): `Warden.LocalControl` is the keyless handler. `serveControl` in `infermux_control.go` binds the socket. The client is `internal/hold`, and the command is `cmd/infermux-hold`. The module's `services.infermux.controlGroup` turns on both and puts `infermux-hold` on the PATH. "Clear" means `/warden/unload` answered 200 and `/warden/resources` shows `our_vram_mb` at 0. A daemon that is not running (no socket, or connection refused) holds no models, since they are its children, so the command starts with a warning.

## Consequences

- The by-hand unload skips `interactive_recent_seconds`. A model used a minute before the launch is unloaded, and the next request loads it again. Launching a game is the user's own statement that the card is wanted.
- `/warden/unload` stops every model, the CPU device's too (0022), although those hold no VRAM.
- A manual pause ends once the warden's own verdict changes (README). With the game's load at or above 25%, the warden pauses on its own and keeps the card until 300 s after the game exits. A game below 25% keeps the manual pause until `infermux-hold` exits. If `infermux-hold` is killed with SIGKILL before the game creates load, the pause stays until the next verdict change or a hand `auto`. That is the 0001 residual risk in a new form, not mitigated here.
- An interactive request during the game can still load a model (0017, 3). The game then loses only what it allocates afterwards, not what it placed at start.
- If the user had set a manual pause before the launch, `infermux-hold` leaves it in place at exit.
- Two games under `infermux-hold` at once: the first to exit sets `auto` under the second. Only a game below 25% load would notice.
- The daemon joins `controlGroup` to hand it the socket. On strix that group is `users`, so the user's existing session can open the socket without logging in again.
- Handing the socket to the group is an `fchownat`, which the unit's `~@privileged` filter forbids. The first deploy on strix, 2026-10-12, was killed with SIGSYS at start. The user chose to allow `@chown` when `controlGroup` is set, over a systemd socket unit (no chown and no group for the daemon, but a connection to a stopped InferMux would start it). A dynamic user can only chown its own files to groups it is in.

Verified: `TestLocalControlTakesNoKeyAndServesOnlyTheControls`, `TestControl_TheSocketIsTheGroupsOrOnlyOurs` and `internal/hold`'s five tests. A keyless handler that also served the keyed routes failed the first, and a release that cleared the user's own pause failed `TestHold_AUsersOwnPauseIsLeftInPlace`.
