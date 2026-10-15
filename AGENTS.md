# AGENTS.md

InferMux (llama-warden until 2026-10-03): llama-swap's model router with a GPU warden built in. One systemd service on the NixOS host, on :5001. It starts the model servers on demand, measures what else is using the card, decides whether that other work is at stake, unloads the models, frees an idle ComfyUI, refuses and cancels batch requests, and pushes `pause`/`resume` to its consumers. The user's own requests are never killed.

This file is rules and navigation only, with llama-swap's own guide below them. `CLAUDE.md` is upstream's one line, `@AGENTS.md`, so Claude reads this file too ([0027](docs/decisions/0027-one-agents-md-ours-above-upstreams-guide.md)).

- [README.md](README.md) is how to run and configure it.
- [docs/decisions/](docs/decisions/README.md) is why anything is the way it is. **`(0001)` means `docs/decisions/0001-*.md`.** Grep it before changing something that looks arbitrary; add to it when we decide something new. [0004](docs/decisions/0004-infermux-request-priority.md) is the merge with llama-swap and the request priority.
- [docs/verification.md](docs/verification.md) is what is built but not yet watched running. Read it before claiming a path works.
- [llama-swap's own guide](#llama-swaps-own-guide), at the end of this file, is upstream's guide to its code. Follow it when touching anything outside `internal/warden/` and `infermux.go`.
- [graphics/README.md](graphics/README.md) is what the mark means.
- The NixOS configuration that deploys this is `~/Projects/nixcfg` (`modules/nixos/llm.nix` for InferMux and the model presets, `packages/comfyui` for ComfyUI).

## Map

| path | what it is |
|---|---|
| `internal/warden/` | ours: config, policy, probe, the CPU's meter and each model's devices (`cpu.go`, `devices.go`, 0022), the fit check at a start on the GPU (`fit.go`, 0028), ComfyUI, consumers, request classes, stuck agents, the loop and `/warden/*` |
| `internal/remote/` | ours: the other InferMux hosts' models, discovered and forwarded to (0006) |
| `internal/placement/` | ours: the one config (0022): hosts, devices, groups, the `balance` and `failover` lanes, a model's `vram_mb` (0028), and each host's part derived from it, llama-swap's file included and loaded with the host's `-config` |
| `internal/configstore/`, `cmd/infermux-config/` | ours: the config's signature, the daemon's kept versions and conflicts, and the terminal tool that makes the key and signs (0022) |
| `infermux_config.go` | ours: the daemon's side of the one config: start, `/warden/config*`, push and poll between hosts, and a derived file that does not load moved aside before llama-swap reads it (0022, 11) |
| `internal/cpurun/`, `cmd/infermux-cpu/` | ours: the runner that starts a CPU device's model servers in the root `idle.slice`, as its children rather than the daemon's (0022) |
| `internal/fetch/` | ours: a local model's missing file, fetched from another host or Hugging Face and checked against the signed SHA-256; the gate that answers 503 until it is here (0022, 8) |
| `internal/failover/` | ours: failure and capacity overflow between model destinations (0016, [0019](docs/decisions/0019-model-overflow-uses-destination-capacity.md), 0021); admission counts each serving host's requests after authentication; `mark.go` marks llama-swap's own 500 for a request no model had, the one 500 that fails over |
| `internal/catalog/` | ours: each local model's context, input and efforts, derived from its command and GGUF, a cloud peer's read from its own list, and Codex's catalog format (0007, 0010) |
| `internal/jobs/` | ours: the job envelope (0023); a batch request InferMux pauses mid-reply on a yield and continues from its text, each attempt sent through failover in-process; `/warden/jobs`; written to disk only on SIGTERM |
| `internal/drain/` | ours: a deploy's stop (0026): the outermost handler, 503 `draining` for a new request, the running ones waited for up to 40 s, then cut |
| `infermux_off.go`, `internal/warden/off.go` | ours: the off file a safety stop writes, read before the first request and every second; while it is there every local request is refused (0025); the start gate then asks the warden's fit check (0028) |
| `infermux_control.go`, `internal/hold/`, `cmd/infermux-hold/` | ours: the keyless control socket, and the command that clears the card through it before a game starts (0024) |
| `internal/stats/` | ours: each request this host serves, timed to its first token, with llama-server's timings; kept in memory, summarised per model (0014) |
| `internal/stream/` | ours: a WebSocket followed frame by frame for the warden and the stats, closed with a code of ours, and routed by `?model=` (0018); `wstest/` is the tests' frame client and echo backend |
| `infermux.go` | ours: wires the warden in front of whichever llama-swap server is active, and owns the config watchers |
| `internal/adapter/`, `cmd/infermux-adapter/` | ours: puts a client's key on the requests of a client that cannot send one, such as Immich (0013) |
| `internal/muxui/`, `cmd/infermux-ui/` | ours: the UI binary; the one config's check, Publish and nixcfg's copy (`config.go`, 0022), the check's load in a child with the daemon's env file (`envfile.go`, 0022, 11), warden.yaml, keys, git, the proxy to the daemon (0005) |
| `webui/` | ours: the Svelte 5 frontend, embedded into `internal/muxui/dist/` by the nix build; the one config's YAML editor is CodeMirror (`src/lib/editor.ts`, `yamlschema.ts`), tested by bun in `test/` |
| `internal/server/warden.go`, `warden_test.go` | ours: the accessors the warden needs on llama-swap's server |
| `internal/process/warden.go`, `warden_test.go` | ours: the gate every start of a local model passes, set by `infermux_off.go` from the off file (0025) and the fit check (0028); its one call is marked in upstream's `process_command.go`, in `run()` |
| `llama-swap.go` | upstream's `main`, plus one flag and three calls: `setAsideDerived` before llama-swap loads its config (0022, 11), `startWarden`, and the stop it returns, first in the SIGTERM branch and before the backstop's clock (0023, 0026) |
| everything else in Go, `ui/`, `docs/` except `docs/decisions/` | upstream llama-swap, merged at the tag in `nix/package.nix`'s `upstream` |

## Commands

```sh
nix develop -c go test ./internal/warden/ ./internal/remote/ ./internal/failover/ ./internal/catalog/ ./internal/muxui/ ./internal/adapter/ ./internal/stats/ ./internal/stream/... ./internal/placement/ ./internal/configstore/ ./internal/cpurun/ ./internal/fetch/ ./internal/jobs/ ./internal/hold/ ./internal/drain/ ./cmd/infermux-config/  # no GPU or model needed; sops and age come from the shell
(cd webui && npm run check && npm run build && bun test)   # the frontend; bun runs webui/test
nix develop -c go test -short ./internal/server/ ./internal/process/ .  # upstream's tests where we touch it
nix develop -c gofmt -l infermux.go infermux_config.go infermux_config_test.go infermux_control.go infermux_control_test.go infermux_loadbalance_test.go infermux_off.go infermux_off_test.go infermux_stream_test.go infermux_jobs_test.go infermux_drain_test.go infermux_main_test.go infermux_test.go infermux_fit_test.go infermux_binary_test.go internal/warden internal/remote internal/failover internal/catalog internal/muxui internal/adapter internal/stats internal/stream internal/placement internal/configstore internal/cpurun internal/fetch internal/jobs internal/hold internal/drain cmd internal/server/warden.go internal/server/warden_test.go internal/process/warden.go internal/process/warden_test.go
nix develop -c go test -run '^$' -bench BenchmarkFrame ./internal/stream/  # what a session costs per frame
nix build                                                  # the package; runs the Go tests above and webui's
e2e/run.sh                                                 # after nix build: the UI and daemon end to end, isolated; reads NVML, loads no model
e2e/run.sh config.py                                       # only the Config tab's editor (run.py is the other pages)

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
git merge v<N>                       # README.md keeps ours (.gitattributes); AGENTS.md and CLAUDE.md merge
```

`git config merge.ours.driver true` once per clone, or `.gitattributes` does nothing. Upstream's changes to its guide merge into the end of this file. One conflicts only where it changes the guide's first line or adds one above it, next to the end of our part: then keep both, ours above (0027). Then bump `upstream` and `vendorHash` (and the UI's `npmDepsHash` if `ui/package-lock.json` moved) in `nix/package.nix`, and check `go.mod`'s Go version against nixpkgs.

## The rules that have to fire without being looked up

- **Every client has a key once `keys_file` is set, and keys.yaml holds only hashes** (0006). The plaintext lives in the hosts' sops file and nowhere else, and never on a command line. A job's key stays in memory, so `jobs.json` never holds one (0023). The clouds' keys in the daemon's env file reach the UI only in the environment of its loader child, never its own, which git and nix inherit (0022, 11).
- **A config is trusted by its signature and by nothing else** (0022, 10). The trusted keys come from Nix's command line, never from the config or a file the UI writes. A daemon never runs a config that fails the check, and the host key carries a signed config without being able to write one.
- **Between hosts the client's own key travels; a host's own key is for discovery and never replaces it** (0006). A llama-swap peer `apiKey` would replace the client's key and with it its class, so the hosts are `remotes`, not `peers`.
- **The user's own request is never killed** (0004), **nor paused** (0023). Unmarked is interactive. Do not add a path that cancels, refuses or unloads under an interactive request; the unload waits for `interactive_recent_seconds` of quiet. The exceptions are a stuck agent's request, refused before it reaches the model, by the user's choice (0015), a safety stop's off file, which refuses and cancels every request for a local model and stops the models at once (0025), and a start on the GPU whose `vram_mb` the card's own free figure does not cover, refused at once (0028).
- **The gate is for this host's devices** (0009; the CPU under the one config, 0022). Only a request for a local model is refused, cancelled or counted as interactive, and only by a device the model is on; another host's and a peer's pass after the key and allow check.
- **Overflow never takes a yielded card** (0021). An `only_if_idle` place on this host's GPU refuses every key while the warden is yielded. A directly addressed request is still the gate's to decide.
- **Upstream files stay untouched except at the marked hook points** (0004). New behaviour goes in `internal/warden/`, `infermux.go`, or a new file in upstream's package. Editing upstream code is a merge conflict we pay every release.
- **No test reads the host's GPU or CPU.** A game on strix's card yields a warden that does, and the test's batch request is cancelled before it begins (sol, review 12). The root's `TestMain` (`infermux_main_test.go`) gives `startWarden` quiet probes; a warden test sets its own or builds the warden with `NewWithProbes`.
- **`internal/warden` imports nothing from llama-swap.** It sees the models through the `Models` interface, which is what keeps it testable without a server and survives a config reload replacing the server.
- **The machine that measures is the machine that decides** (0001). A threshold that lives in a consumer's config is the thing this project was created to end.
- **A pause is a message, not a lease.** Nothing expires, so a warden that dies while a consumer is paused leaves it paused. The residual risk is stated in 0001. Do not add a second, quieter mitigation without reading that section; the real fix, if it is ever needed, is the lease.
- **An announcement must be idempotent at the other end.** The verdict is re-sent every 300 s, so a consumer that re-stamps `since` on every `pause` erases the one fact its panel shows.
- **A failed probe leaves the verdict alone, and so does an unreadable ComfyUI queue.** Neither is evidence that the GPU is free.
- **A device's models are unloaded once per yield of that device** (0002), owed from the transition and paid when interactive traffic on that device is quiet (0004). The CPU's are unloaded only for RAM under the floor (0022). Unloading every tick would fight a client the user chose to let through.
- **Free VRAM is only read while no GPU model is loaded.** Kept, not re-decided (0002). `starting` counts as loaded (0001). A CPU model holds no VRAM (0022).
- **A WebSocket counts toward the warden only while data frames cross** (0018). An open connection and a ping count for nothing. Anything that stops a model under a session closes it first with 1013 or 1012; the backend's death is a 1006 the client cannot tell from a crash.
- **A writer of ours that records or acts on a status treats a 1xx other than 101 as interim** (0016). The status after it is the answer's. llama-swap's own writers keep the first status they see, so Mark removes `Expect` before the request reaches them.
- **A stop's SIGTERM is the daemon's alone** (`KillMode=mixed`, 0026), so a deploy can drain. Whatever must take a model off the card at once kills the model, never only the unit; a stop while off drains too.
- **A llama-swap config reload waits for quiet** (0005). It stops every model, so it goes through `WhenNoInteractive`, never `-watch-config`.
- **The UI never pushes, and commits only when the user presses Commit** (0005).
- **ComfyUI is contention, never ours.** It is not in `our_units` and it is not a consumer. It is a tenant the warden watches and frees (0002). `comfyui_unit` names it so the config loader refuses it in `our_units`.

## Engineering principles (user feedback, hard)

- **Fix root causes, not symptoms.** A consumer-side workaround is acceptable only as an explicitly temporary bridge, agreed with the user.
- **Don't self-authorize known design debt.** Surface the smell and the proper fix; the user decides.
- **Build the simplest mechanism that satisfies the stated requirement.**
- **"Tests pass" and live verification are different claims.** Say which one was done.

## llama-swap's own guide

Everything below is llama-swap's `AGENTS.md` as upstream ships it, kept verbatim so that upstream's changes merge (0027). Edit above this heading, never below it.

## Architecture

Only use these technologies:

- Go 1.26+
- Typescript, Vite and Svelte 5 for UI (ui/)
- Docker
- Kubernetes (API, e.g. client-go) for cmd/kubeswap ONLY
- Markdown
- YAML
- Makefile
- bash

## Contribution Guidelines

- Follow CONTRIBUTING.md guidelines before submitted an issue or pull request (PR)
- When asked to create a pull request or issue, always double check no violations of CONTRIBUTING.md. Ask the user for a decision when there is a violate.

## Testing Changes

- Use test naming conventions like `TestProxy_<test name>`, `TestProcessGroup_<test name>`, etc.
- Use `go test -v -run <new tests>` to quickly check new tests
- Use `make test-dev` after any changes to Go source
- Use `make test-ui` after any changes in ui/
- Use `make test-all` for commiting changes
- Use the ./build subdirectory for testing binary builds
- Only when the user asks use `make eval-docs-agent` to score the
  Help page's Docs Agent against a local model after changing its system prompt, `docs/kb/` content, the MCP tool descriptions, or the docs search ranking. See `evals/docs-agent/README.md` for the tuning loop.

## Documentation

- When a change adds, removes, or changes the meaning of a configuration
  option, add or update a knowledge base guide under `docs/kb/guides/` for
  it, not just `docs/config.example.yaml` and `config-schema.json`.
- Prefer extending an existing guide covering the same topic over creating a
  new file for a single setting.
- Follow the frontmatter contract and writing guidelines in `docs/kb/README.md`
  (required `title`/`summary`/`category`, `config_keys` referencing real
  schema keys, keep it short, show a working config, say what goes wrong).
- Run `make test-dev` afterward; `TestKB_FrontmatterIsValid` in
  `internal/docagent` checks the frontmatter and that `config_keys` resolve.

### git commit rules

- Run `gofmt -w <file>` before committing to fix any formatting
- Use this format for commit messages:
- When referencing issues use "fix: #123", "update: #123"
- Use "fix" when the branch resolves an issue
- Use "update" when the branch only contributes to the issue
- Hardwrap commit messages to 80 characters wide

```
internal/server: short clear description of change

Add new feature that implements functionality X and Y.

- key change 1
- key change 2
- key change 3

fix: #123
update: #456
```

## Code Reviews

Follow these rules when performing a code review

- Use severity levels: High, Medium and Low
- Tag issues with a severity and number like: H1, M2, L3
- High severity are must fix issues: security, race conditions, logic errors
- Medium severity are recommended improvements: coding style, missing tests, inaccurate comments
- Low severity are nice to have changes
- Include a suggestion for high and medium severity items
- Limit your code review to three items sorted by severity
