# 0022. One signed config for every host, with models placed on devices

- Date: 2026-10-10
- Status: proposed; nothing built
- Would replace [0016](0016-a-model-fails-over-between-places.md) and [0019](0019-model-overflow-uses-destination-capacity.md)'s `failover.yaml`, [0011](0011-the-cpu-embedder-is-a-peer.md), and parts of [0005](0005-web-ui-and-config-outside-nix.md) and [0008](0008-the-zbox-has-its-own-ui.md). Keeps [0009](0009-the-gate-is-for-this-hosts-card.md), [0017](0017-a-priority-process-takes-the-card.md) and [0021](0021-overflow-never-takes-a-yielded-card.md).

## Context

On 2026-10-10 the user could not disable the CPU embedder and called the load balancing illegible. Turning off that one route meant editing up to five places: the `llama-embed` unit in nixcfg's `llm.nix` (a rebuild), the `embed-cpu` peer in `models/embed-cpu.yaml`, the third place in `failover.yaml` (which the zbox reads through a symlink, so only after a push and its pull timer), and a hand-written unit described below. The UI showed none of the routing. It never read `failover.yaml`, peers were a textarea, and each host's UI listed only its own models. `failover.yaml` named its places three ways: `zbox`, `strix`, `strix/embed-cpu/Octen-Embedding-4B.Q8_0`.

A separate fault made it worse. `/etc/systemd/system.control/llama-embed.service`, written 2026-10-08 01:39 with its own `multi-user.target.wants` link, shadowed the Nix unit (`FragmentPath` pointed at it). No change in `llm.nix` could stop the embedder, and the 2026-10-09 `cpu-inference.slice` fix never applied: the running embedder was in `system.slice` at nice 0.

The agent offered three options: a page over `failover.yaml`; that page plus InferMux running the CPU server; or one config from which every host's models and routing are derived. The user chose the last and described it. There is one InferMux per computer. Each has several devices, such as one GPU and one CPU, and a device runs whichever runtime a model needs, mainline llama.cpp or a fork. Balancing decides where a model runs normally, and failover keeps some service when a host or device goes down. The user adds models or model groups to the config: the embedder on the GPU and the CPU, a 7B chat model on the GPU only. The same file goes to every host, each host reads its own part, and a change applies at runtime.

## Decision

The user's choices, 2026-10-10:

1. **One config, the same on every host, and each host runs its own part.** A host has devices; a model is placed on devices.
2. **Two lanes per model, `balance` and `failover`.** Balance is the normal places, in order, each up to its limit, as 0019's overflow works today. Failover places are used only when every balance place is down or refusing, not when one is merely busy. Rejected: a failover rule per host or device ("zbox/gpu's models move to strix/gpu"); two separate files.
3. **A model group is a placement label.** Placing a group places every member. Clients still ask for a model by name. Rejected: a group as a name clients ask for, which changes the API; no groups.
4. **The daemons push the config to each other, with a revision.** Each daemon keeps its copy under its state directory, not in the checkout, because the zbox's `nixcfg-pull` stops on an uncommitted change and a pushed file would be one. The UI writes a copy to nixcfg for history, committed with Commit as now. Rejected: git as the transport, which means the UI pushes (reversing 0005), the passphrase on every change, and no change while GitHub is down.
5. **A model is defined once.** The device adds its own selection, and a placement may override a flag, such as a smaller `--ctx-size` on the 1060. Rejected: a full command per device.
6. **Two hosts edited apart: the last write wins, with a warning and a backup.** The later signed timestamp wins, ties broken by host name. The losing version is kept on both hosts, and both UIs show it as a diff with a warning until the user dismisses it. Rejected: holding both versions until the user picks; refusing changes while any host is unreachable, which takes away the reason the zbox has its own UI (0008).
7. **Each device runs one model or several at once, as a setting on the device.** The GPUs swap one at a time as now; the CPU, with 96 GB, can keep several running. This is llama-swap's group `swap` flag, so no upstream file changes. Rejected: a memory budget per device, which needs reliable sizes per model and context; one model at a time everywhere.
8. **A host fetches a missing GGUF from another host that has it, or from its source** (Hugging Face, 0012), and skips that placement until the file is complete. Rejected: refusing the change until the file is there.
9. **A YAML editor, not drag and drop.** The user asked for an editor with help built in: completion and hover docs from a schema generated from the Go config types (device names, model names, runtimes, GGUF paths included); validation as you type by the loader the daemons run; commands that add a model from an unused GGUF or a Hugging Face repo, with flags copied from a model on the same runtime. The editor replaces the model form, the Peers textarea and the Routing textarea. The KV-kernel warning becomes a diagnostic on the cache-type line. Rejected: keeping the form beside the editor, two editors writing one file.
10. **The config is signed with a dedicated key, and verification is forced.** An ed25519 key used for nothing else. Every daemon checks the signature against public keys from Nix (`services.infermux`), never from the config, since a config that names its own trusted key proves nothing. A daemon never applies a config that fails the check. It keeps running the last one that passed, and a new host with none runs no local model until a signed one arrives. The host key gains no power: it reads the other host's model list and carries a signed config, and cannot write one. Rejected: the user's GPG key, where a leak of either the config or the commit key would give both; each host's own key, which lets a compromised host rewrite every host.

## Proposed by the agent, agreed by the user the same day

- **"Device"** for the hardware unit, llama.cpp's own word (`--device`, `CUDA0`). "Resource group" would clash with llama-swap's `groups`.
- **The config is a directory, signed as one unit:** `infermux.yaml` for hosts, devices, clouds, groups, `balance` and `failover`, and `models/<name>.yaml` for each definition, which keeps each model's dated notes in its own file. The signature covers a manifest of the revision, the timestamp, the host that published it, and the SHA-256 of every file.
- **Each model records its GGUF's SHA-256 in the config.** A file fetched from another host is checked against what the user signed. Without it a compromised host could hand llama.cpp's parser a crafted file, which is what forced verification is meant to prevent. The add-model command fills it in from Hugging Face's metadata or by hashing the local file.
- **Another host first, then the source,** for a missing GGUF. The tailnet is closer than Hugging Face, and the hash makes the order safe.
- **Editing is a draft; Publish signs and pushes.** Validation runs on the draft. The private key is age-encrypted with a passphrase (scrypt) in nixcfg, and the UI decrypts it in memory for one Publish with the passphrase from the browser. That works on the zbox, which has no age identity (0008).
- **A device decides its hardware by environment, not only by flags.** A GPU device sets `CUDA_VISIBLE_DEVICES=<index>` and a CPU device sets it empty and adds `--device none -ngl 0`, so a stray `-ngl 99` in a model cannot reach a card it was not placed on. This answers 0011's objection to CPU models under InferMux, that a GPU model filed under the CPU by mistake would hide from the warden. The empty variable also covers raw commands such as the reranker and immich-ml.
- **The warden's rules apply per device, not per host.** 0011's three rules (unload on a yield, free VRAM read only with nothing loaded, batch refused in a pause) were why a CPU model could not live under InferMux: they treated every local model as the card's tenant. Per device, a GPU yield unloads and gates only that GPU's models, and the VRAM rule stays a GPU rule. The CPU's own rules are in "The warden covers every device" below; this paragraph, as first written, gave the CPU none.
- **CPU devices run their servers in an idle slice at the root of the cgroup tree,** through `Delegate=` on `infermux.service` or a transient unit. Not `cpu-inference.slice` as nixcfg names it today: systemd reads a dash in a slice name as nesting, so it is `_cpu.slice/cpu-inference.slice`, and its `cpu.idle` ranks it only inside `cpu.slice`, which competes with `user.slice` at weight 100. Measured 2026-10-10 with a busy loop in each, both pinned to core 23 for 10 s: in `cpu-inference.slice` 23% of the core against 76% in `user.slice`; in a slice at the root set to `CPUWeight=idle`, 0% against 98%.
- **What stays as it is:** `warden.yaml`'s per-host policy (desktop and priority processes, ComfyUI, consumers) in the first version; `<host>/<model>` addressing a request to one host; a yielded card dropping out of `balance` (0021); `batch_only` and `only_if_idle` as conditions on a placement.

## The warden and the config

The user asked, 2026-10-10, whether the warden is the host's local rules for what the host must put first so its other tasks keep running, applying only to processes outside InferMux, with InferMux's own work governed by the config. The first half is right, the second the wrong way round, and the split that follows is the one this design keeps.

**The config says where InferMux's work may run. The warden says whether one host's GPU takes it right now.** The warden is local because the machine that measures is the machine that decides (0001). It protects the host's other GPU work, but it acts on InferMux, not on that work. It unloads InferMux's models, refuses and cancels InferMux's batch requests, and pauses InferMux's consumers. A game or OBS is never touched. The one outside process it acts on is an idle ComfyUI, which it frees so the card can be measured as free (0002).

The order on a GPU is: the user's own request, which is never killed even when a game wants the card (0004); then the host's other GPU work; then InferMux's batch work. The warden outranks the config. A yielded GPU device drops out of `balance` (0021), and the config cannot place work on it. The warden's GPU rules never send work anywhere. They only refuse, unload, cancel and pause, and the config's lanes decide where refused work goes next. `stuck.escalate_to` (0015) is the exception, a routing choice that lives in `warden.yaml`; whether it moves into the config is open.

## The warden covers every device

The user, 2026-10-10: the warden should cover the CPU too, so it works for every device InferMux may use. The loop is the same for each device: measure, decide busy or free, act. A busy device drops out of `balance` (0021 for every device), and the warden refuses and cancels batch work on its models. The user's own request is never killed (0004). Each device has its own verdict and its own `resume_quiet_seconds`.

The warden does not replace the idle cgroup. It polls every few seconds, while the scheduler decides every few milliseconds, and the 2026-10-09 desktop lag was a scheduling problem a poll cannot catch. The cgroup decides who gets a core at each instant. The warden decides whether work is sent to the CPU at all.

The user's choices:

1. **A busy CPU routes and refuses, and the warden unloads only when RAM runs short.** An idle CPU model holds RAM and no CPU time, so a yield on CPU load unloads nothing. Under a floor of available RAM the warden unloads the CPU device's models, as on a GPU once interactive traffic is quiet (0004). Rejected: no unload at all; unloading on every CPU yield, which frees RAM nobody asked for and makes the next request pay the load. To measure first: llama.cpp maps the GGUF, and mapped pages are page cache the kernel can reclaim and `MemAvailable` already counts, so an unload frees mostly the KV cache and compute buffers. The floor has to be set against that number.
2. **The CPU is busy when CPU time used outside InferMux's cgroups crosses a threshold for a window, or when a priority process runs.** The kernel charges every nanosecond to a cgroup, so this is exact and readable while models run, unlike free VRAM (0002). Rejected: CPU pressure (PSI) in `user.slice`, which with InferMux idle-weighted mostly fires when the desktop starves itself; both together.
3. **A placement declares every device it occupies.** A model with `--fit` runs the layers that do not fit in VRAM on CPU threads: the 27B has 26 of its 66 layers on the 3080 (`models/README.md`, 2026-08-15). It is placed as occupying the GPU and the CPU, and a busy CPU refuses its batch work too. The editor's validation warns when `--fit`, a low `-ngl` or `--cpu-moe` suggests CPU use the placement does not declare. Rejected: inferring it from the flags, which cannot say how much, since `--fit` splits at load time; counting such a model on its GPU only, which leaves the desktop unprotected from its threads, as today.
4. *Replaced the same day by [0023](0023-a-batch-request-is-a-job-paused-mid-inference.md): first-party consumers send jobs that InferMux pauses and resumes, and nothing is pushed to them.* **A consumer is paused when one of its models has no place, on any host, that can take batch work now.** A consumer names the models it uses. Each host's warden needs the other hosts' device verdicts, which the discovery poll can carry. Rejected: pausing on this host's GPU only, as today; pausing on any device of the host, which would stop Episteme's GPU work when only the CPU is busy.

Defaults the agent chose, open to change:

- On a CPU every process outside InferMux counts, the desktop included. `desktop_processes` exists because the compositor's steady 30-38% of the GPU is not a reason to yield. On the CPU the desktop is the work being protected, and the threshold absorbs its background.
- A model occupying the GPU and the CPU keeps normal CPU weight, and only CPU-device models run in the idle slice. Putting the 27B's threads at idle weight would make the user's own chat crawl whenever the desktop is busy. The warden refusing its batch work is what protects the desktop from it.

## Consequences

- llama-swap's YAML is no longer the file format (0005, 3). Each host generates llama-swap's config from its part: its models, and one group per device.
- Retired: both hosts' `models/*.yaml` as llama-swap files, `failover.yaml`, the `embed-cpu` and `openrouter` peer files, the Routing textarea, `warden.yaml`'s `remotes`, and `llama-embed.service` in `llm.nix`. Nix keeps the runtimes as macros, the keys and secrets, and the trusted config keys.
- Changing which keys are trusted needs a rebuild. That is the point.
- The nixcfg copy carries its signature, so it can start a fresh host.

## Later, perhaps

- **Live state in the editor's margin:** beside each model and placement, running or stopped, the host and device it is on, median TTFT and tok/s from the Performance data. The user, 2026-10-10: a possible later addition.

## Build order

0. Remove `/etc/systemd/system.control/llama-embed.service` and its wants link, so the Nix unit and its idle slice apply. Done 2026-10-10, the user agreeing; the file is kept in `~/Projects/scratch/system.control-backup-2026-10-10/`. After a restart the embedder runs from the Nix unit in `cpu-inference.slice` at nice 19, but its threads are `SCHED_OTHER`, not the unit's `CPUSchedulingPolicy=idle`, and the slice is nested (above). Moving the slice to the root is nixcfg's fix and does not wait for the rest.
1. The config format, its loader and each host's derivation, as pure Go: llama-swap models and groups, the two lanes, the warden's scope. Tested against today's files translated by hand.
2. Signing and verification, the trusted keys in the Nix module, the daemon's state directory, start from the nixcfg copy.
3. Sync between the daemons: push on Publish, compare revisions on the discovery poll, last write wins with the backup and the warning.
4. Devices: the environment and flags, the CPU device in the idle slice. Retire `llama-embed.service` and `embed-cpu`.
5. The failover lane in `internal/failover`.
6. Fetching a GGUF from a host or the source, checked against its hash.
7. The editor: the schema, the validation endpoint, the add-model commands, Publish, the conflict warning. Remove the model form, Peers and Routing.
8. Move both hosts over, and add status lines to 0005, 0008, 0011, 0016 and 0019.

## Calls the agent made while building, for the user to review

The user, 2026-10-10: "you may drive this to completion on your own. but note down any decisions that would normally be surfaced to me first". Each of these is open to change.

### The format (`internal/placement`)

- **A model file is a llama-swap model entry plus `files:`.** `models/<name>.yaml` holds what a `models.<name>` entry held (`cmd`, `env`, `ttl`, `capabilities`, ...) without the `models:` wrapper, so today's files move over nearly unchanged and llama-swap's own loader checks them. `files:` maps each weight's path to its `sha256` and optional `hf` source (0012's `org/repo/file`). A GGUF named in the command but missing from `files:` is a warning, not an error.
- **A model on two devices of one host has two names there.** Its first place on that host, balance before failover, serves the model's own name; any further device serves `<model>@<device>`, unlisted and without aliases. The embedder on strix is `Octen-Embedding-4B.Q8_0` on the GPU and `Octen-Embedding-4B.Q8_0@cpu` on the CPU, and the zbox routes to the second as `strix/Octen-Embedding-4B.Q8_0@cpu`.
- **A model's own lane entry replaces its group's, per lane.** A model in two groups that both have an entry in the same lane is refused, since their order would be a guess.
- **A placement's `flags` match the exact spelling.** A value sets the flag's values, `true` keeps it bare, `false` or null removes it. `-c` does not override `--ctx-size`. The device's flags apply after the placement's, so no override puts a CPU placement's layers on a card.
- **The CPU device's `--device none -ngl 0` go only to llama.cpp commands,** those with a `--model` ending in `.gguf`. The reranker and immich-ml take other flags; `CUDA_VISIBLE_DEVICES=` alone keeps them off the card. Every other spelling of `-ngl` and `--device` is removed first.
- **`concurrent` defaults to false on every device,** so a device runs one model at a time unless the config says otherwise. Each device is a llama-swap group with `exclusive: false`, so a model starting on one device never stops another device's.
- **Clouds are offered by every host,** as llama-swap peers, with the key taken from each host's environment through `${env.VAR}`.
- **A model placed nowhere is a warning, not an error.** Removing a model's places is how it is turned off without deleting its file and notes.
- **A host the config does not list runs no model of its own** and still routes to the others.

### Signing and the store (`internal/configstore`, `cmd/infermux-config`)

- **The manifest is JSON with a `parent`,** the hash of the manifest it was edited from. A config whose parent is the one running replaces it quietly; any other later one replaces it as a conflict. Without the parent a host could not tell an edit made apart from a plain update.
- **A tie in signed time goes to the greater host name,** `zbox` over `strix`. Any fixed order would do.
- **A signed time more than 10 minutes ahead of the host's clock is refused.** The later time wins every conflict, so a config stamped a year ahead would beat every edit for a year.
- **An older nixcfg copy at start is ignored, not reported.** The copy lags whenever a Publish has not been committed, so treating it as an edit made apart would raise a warning on most restarts. An older config from another daemon is still a conflict.
- **A public key is its 32 bytes in base64,** one a line in the file Nix writes, with an optional comment after it. The private key file is age's scrypt format holding the seed in base64, so `age -d` opens it by hand too.
- **The daemon keeps the last 20 versions** under `versions/`, besides the current one and both sides of an open conflict.
- **`infermux-config` signs from a terminal:** `keygen`, `sign` (checks the config as a daemon would and refuses to sign one that fails), `verify` and `derive`. The migration needs it before the editor exists, and it stays useful when no browser is at hand.
