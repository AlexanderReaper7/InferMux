# 0005. A web UI as its own process, and the model and warden files moved out of the Nix store

- Date: 2026-10-03
- Status: accepted
- Rule: `infermux-ui` is a separate binary, run as the user's systemd user service on 127.0.0.1:5010. It edits `models/*.yaml` and `warden.yaml` in a directory outside the store (`services.infermux.configDir`) and commits them only when the user presses Commit; it never pushes. The daemon reloads the warden file at once and llama-swap's config only once no interactive request is in flight. A manual pause or resume holds until the warden's own decision changes. Browser writes are taken only same-origin on a loopback name, and `/warden/*` writes need an `X-InferMux` header.
- Keeps 0004's rule that the user's own request is never killed: a config reload restarts every model, so it waits behind interactive traffic like the unload does.

## Context

The user asked for a GUI that is separate from the daemon, from which everything can be configured, plus controls and a small status applet. A native desktop app and an Android app may come later; for now it runs on this desktop only.

Until now every model and every warden key lived in nixcfg as Nix and INI, and any change was a `nixos-rebuild switch`. The user asked whether a full rebuild is needed to add a model or tune a parameter. For llama-swap's model entries it is not: llama-swap merges `-config` with every `*.yaml` in `-config-dir` and can reload them. What does need Nix is the store paths of the runtimes, so those stay in the base config as macros (`${llama-server}`, `${bonsai-server}`).

## Decision

The user's choices, 2026-10-03:

1. **A web UI first, Svelte 5 and Tailwind, served by its own Go binary.** It is a separate process so the daemon carries no UI state and a native or Android client can later talk to the same HTTP API. The daemon's endpoints stay the API for status and controls; the UI binary adds file editing, which the daemon cannot do because it runs as a `DynamicUser` with the config dir bound read-only. The UI proxies `/daemon/*` to :5001, so the browser talks to one origin.
2. **The UI edits files, the user commits them by hand.** One llama-swap YAML file per model under `configDir/models/`, and `configDir/warden.yaml`. The UI edits through yaml.v3's node tree, so comments and keys it does not know survive a save. Every save is validated with llama-swap's own `config.LoadConfigSources` against the base config before the file is replaced, and written through a temporary name that does not end in `.yaml`, so the watcher never reads half a file. The Changes tab shows `git status` and the diff and commits only those paths. Rejected: the UI committing every save, and the UI pushing.
3. **The model command is a table when it can round-trip.** A `cmd` of the form `${runtime} --port ${PORT} --model <gguf> <flags...>` opens as runtime, GGUF and a flag table. Anything else, or a flag the table could not write back unchanged, opens as text. The user chose llama-swap's own YAML as the file format over a format of our own.
4. **The reload waits for quiet.** InferMux owns the file watchers instead of passing `-watch-config`. A change to the warden file calls `Reload`, which keeps the verdict and what each consumer has heard. A change to the base config or the models dir is queued under `WhenNoInteractive` and sends the process `SIGHUP` once no interactive request is in flight. llama-swap's reload calls `Shutdown`, which stops every model, so reloading under the user's prompt would kill it. SIGHUP is llama-swap's existing reload path, so no upstream file changed for this. `/warden/verdict` lists what is waiting as `waiting_for_quiet`.
5. **A manual verdict holds until the next transition.** `POST /warden/manual {"action":"pause"|"resume"|"auto"}`. The warden keeps deciding (`own`) and the manual verdict is the effective one until `own` changes its action, at which point the manual one ends with a log line. `auto`, or asking for what the warden already says, ends it at once. Rejected: a hold with a timer, and a hold until cleared by hand, which is a pause nobody remembers making. Manual verdicts are refused while the warden is disabled.
6. **Controls.** `/warden/forgive` drops an owed unload, `/warden/unload` unloads now, `/warden/cancel-batch` cancels batch requests in flight, `/warden/comfyui/free` frees ComfyUI now. `/warden/unload` answers 409 while an interactive request is in flight, which is 0004's rule applied to a button.
7. **Restrict CORS and require a header.** CORS alone does not stop a page on another origin from sending a simple POST, and a DNS-rebinding page can make its origin look like ours. So any POST that carries an `Origin` must be same-origin with a loopback `Host` (`localhost`, `127.0.0.1`, `::1`), on the daemon and on the UI. Writes under `/warden/*` and every UI write also need `X-InferMux`, which a cross-origin page cannot set without a preflight that CORS refuses. The UI answers 421 to any non-loopback `Host`. The module sets llama-swap's `security.cors.allowedOrigins` to the daemon's own origin by default.

The daemon's sandbox changed: with `configDir` set, `ProtectHome=tmpfs` plus `BindReadOnlyPaths=configDir`, instead of `ProtectHome=true`, so a directory under `/home` is visible to it read-only and nothing else in `/home` is.

## What the user gives up, and the open items

- **A browser page on another origin can no longer POST to the models.** The Origin check covers every POST, `/v1/chat/completions` included. Clients that are not browsers send no `Origin` and are unaffected. No such browser client is in use here; a list of allowed origins is the fix if one appears.
- **The KV-cache kernels are still built from the files.** nixcfg reads the `--cache-type-k`/`-v` pairs out of `models/*.yaml` to decide which FlashAttention quantizations llama.cpp compiles. A pair changed in the UI works at once, but on the slow path until the next rebuild. The UI does not warn about this.
- **A reload stops a batch request in flight.** Only interactive traffic delays it.
- **The UI is only for this user on this host.** It runs as one user and refuses non-loopback hosts. A phone client needs authentication first.
- **`infermux-ui` links NVML** through its import of `internal/warden` for the config types and the origin check. It does not call it.
