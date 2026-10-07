# CLAUDE.md

InferMux (llama-warden until 2026-10-03): llama-swap's model router with a GPU warden built in. One systemd service on the NixOS host, on :5001. It starts the model servers on demand, measures what else is using the card, decides whether that other work is at stake, unloads the models, frees an idle ComfyUI, refuses and cancels batch requests, and pushes `pause`/`resume` to its consumers. The user's own requests are never killed.

This file is rules and navigation only.

- [README.md](README.md) is how to run and configure it.
- [docs/decisions/](docs/decisions/README.md) is why anything is the way it is. **`(0001)` means `docs/decisions/0001-*.md`.** Grep it before changing something that looks arbitrary; add to it when we decide something new. [0004](docs/decisions/0004-infermux-request-priority.md) is the merge with llama-swap and the request priority.
- [CLAUDE-TODO.md](CLAUDE-TODO.md) is what is built but not yet watched running. Read it before claiming a path works.
- [AGENTS.md](AGENTS.md) is llama-swap's own guide to its code. Follow it when touching anything outside `internal/warden/` and `infermux.go`.
- [graphics/README.md](graphics/README.md) is what the mark means.
- The NixOS configuration that deploys this is `~/Projects/nixcfg` (`modules/nixos/llm.nix` for InferMux and the model presets, `packages/comfyui` for ComfyUI).

## Map

| path | what it is |
|---|---|
| `internal/warden/` | ours: config, policy, probe, ComfyUI, consumers, request classes, stuck agents, the loop and `/warden/*` |
| `internal/remote/` | ours: the other InferMux hosts' models, discovered and forwarded to (0006) |
| `internal/failover/` | ours: a model listed in failover.yaml tried at each of its places in turn, in front of the warden (0016) |
| `internal/catalog/` | ours: each local model's context, input and efforts, derived from its command and GGUF, a cloud peer's read from its own list, and Codex's catalog format (0007, 0010) |
| `internal/stats/` | ours: each request this host serves, timed to its first token, with llama-server's timings; kept in memory, summarised per model (0014) |
| `internal/stream/` | ours: a WebSocket followed frame by frame for the warden and the stats, closed with a code of ours, and routed by `?model=` (0018); `wstest/` is the tests' frame client and echo backend |
| `infermux.go` | ours: wires the warden in front of whichever llama-swap server is active, and owns the config watchers |
| `internal/adapter/`, `cmd/infermux-adapter/` | ours: puts a client's key on the requests of a client that cannot send one, such as Immich (0013) |
| `internal/muxui/`, `cmd/infermux-ui/` | ours: the UI binary; file editing, validation, git, the proxy to the daemon (0005) |
| `webui/` | ours: the Svelte 5 frontend, embedded into `internal/muxui/dist/` by the nix build |
| `internal/server/warden.go`, `warden_test.go` | ours: the accessors the warden needs on llama-swap's server |
| `llama-swap.go` | upstream's `main`, plus one flag and one call |
| everything else in Go, `ui/`, `docs/` except `docs/decisions/` | upstream llama-swap, merged at the tag in `nix/package.nix`'s `upstream` |

## Commands

```sh
nix develop -c go test ./internal/warden/ ./internal/remote/ ./internal/failover/ ./internal/catalog/ ./internal/muxui/ ./internal/adapter/ ./internal/stats/ ./internal/stream/...  # no GPU or model needed; sops and age come from the shell
(cd webui && npm run check && npm run build)               # the frontend
nix develop -c go test -short ./internal/server/ .         # upstream's tests where we touch it
nix develop -c gofmt -l infermux.go infermux_stream_test.go internal/warden internal/remote internal/failover internal/catalog internal/muxui internal/adapter internal/stats internal/stream cmd internal/server/warden.go internal/server/warden_test.go
nix develop -c go test -run '^$' -bench BenchmarkFrame ./internal/stream/  # what a session costs per frame
nix build                                                  # the package; runs the three test packages
e2e/run.sh                                                 # after nix build: the UI and daemon end to end, isolated; reads NVML, loads no model

# with keys_file set, each of these needs -H "Authorization: Bearer <key>"
curl 127.0.0.1:5001/warden/verdict      # what it decided, who has heard it, what is in flight
curl 127.0.0.1:5001/warden/resources    # a fresh NVML probe
curl '127.0.0.1:5001/warden/requests?hosts=all'  # the last requests' timings, every host's
curl 127.0.0.1:5001/running             # llama-swap: which models are up
journalctl -u infermux -f
```

## Updating llama-swap

```sh
git fetch upstream --tags            # upstream = https://github.com/mostlygeek/llama-swap
git merge v<N>                       # README.md and CLAUDE.md keep ours (.gitattributes)
```

`git config merge.ours.driver true` once per clone, or `.gitattributes` does nothing. Then bump `upstream` and `vendorHash` (and the UI's `npmDepsHash` if `ui/package-lock.json` moved) in `nix/package.nix`, and check `go.mod`'s Go version against nixpkgs.

## The rules that have to fire without being looked up

- **Every client has a key once `keys_file` is set, and keys.yaml holds only hashes** (0006). The plaintext lives in the hosts' sops file and nowhere else, and never on a command line.
- **Between hosts the client's own key travels; a host's own key is for discovery and never replaces it** (0006). A llama-swap peer `apiKey` would replace the client's key and with it its class, so the hosts are `remotes`, not `peers`.
- **The user's own request is never killed** (0004). Unmarked is interactive. Do not add a path that cancels, refuses or unloads under an interactive request; the unload waits for `interactive_recent_seconds` of quiet. The one exception is a stuck agent's request, refused before it reaches the model, by the user's choice (0015).
- **The gate is for this host's card** (0009). Only a request for a local model is refused, cancelled or counted as interactive; another host's and a peer's pass after the key and allow check.
- **Upstream files stay untouched except at the marked hook points** (0004). New behaviour goes in `internal/warden/`, `infermux.go`, or a new file in upstream's package. Editing upstream code is a merge conflict we pay every release.
- **`internal/warden` imports nothing from llama-swap.** It sees the models through the `Models` interface, which is what keeps it testable without a server and survives a config reload replacing the server.
- **The machine that measures is the machine that decides** (0001). A threshold that lives in a consumer's config is the thing this project was created to end.
- **A pause is a message, not a lease.** Nothing expires, so a warden that dies while a consumer is paused leaves it paused. The residual risk is stated in 0001. Do not add a second, quieter mitigation without reading that section; the real fix, if it is ever needed, is the lease.
- **An announcement must be idempotent at the other end.** The verdict is re-sent every 300 s, so a consumer that re-stamps `since` on every `pause` erases the one fact its panel shows.
- **A failed probe leaves the verdict alone, and so does an unreadable ComfyUI queue.** Neither is evidence that the GPU is free.
- **The models are unloaded once per yield** (0002), owed from the transition and paid when interactive traffic is quiet (0004). Unloading every tick would fight a client the user chose to let through.
- **Free VRAM is only read while no model is loaded.** Kept, not re-decided (0002). `starting` counts as loaded (0001).
- **A WebSocket counts toward the warden only while data frames cross** (0018). An open connection and a ping count for nothing. Anything that stops a model under a session closes it first with 1013 or 1012; the backend's death is a 1006 the client cannot tell from a crash.
- **A llama-swap config reload waits for quiet** (0005). It stops every model, so it goes through `WhenNoInteractive`, never `-watch-config`.
- **The UI never pushes, and commits only when the user presses Commit** (0005).
- **ComfyUI is contention, never ours.** It is not in `our_units` and it is not a consumer. It is a tenant the warden watches and frees (0002). `comfyui_unit` names it so the config loader refuses it in `our_units`.

## Engineering principles (user feedback, hard)

- **Fix root causes, not symptoms.** A consumer-side workaround is acceptable only as an explicitly temporary bridge, agreed with the user.
- **Don't self-authorize known design debt.** Surface the smell and the proper fix; the user decides.
- **Build the simplest mechanism that satisfies the stated requirement.**
- **"Tests pass" and live verification are different claims.** Say which one was done.
